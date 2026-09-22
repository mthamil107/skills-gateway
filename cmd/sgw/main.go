// Command sgw is the Skills Gateway server and CLI in one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/config"
	"github.com/mthamil107/skills-gateway/internal/mcp"
	"github.com/mthamil107/skills-gateway/internal/policy"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/server"
	"github.com/mthamil107/skills-gateway/internal/store/sqlite"
	"github.com/mthamil107/skills-gateway/internal/syncer"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const usage = `sgw — Skills Gateway %s

Server:
  sgw serve -config sgw.yaml [-allow-insecure]

Client (uses $SGW_URL and $SGW_TOKEN):
  sgw publish  <skill-dir> -ns <namespace> -version <semver>
  sgw list     [-ns <namespace>] [-q <text>] [-tag <tag>]
  sgw get      <ns>/<name>[@version]
  sgw fetch    <ns>/<name>[@version] [-format claude] [-out .]
  sgw sync     [-file sgw-sync.yaml] [-out .]
  sgw deprecate <ns>/<name>@<version>
  sgw audit    [-since 2026-09-01T00:00:00Z]
  sgw formats
  sgw version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "publish":
		err = publish(args)
	case "list", "search":
		err = list(args)
	case "get":
		err = get(args)
	case "fetch":
		err = fetch(args)
	case "sync":
		err = syncCmd(args)
	case "deprecate":
		err = deprecate(args)
	case "audit":
		err = audit(args)
	case "formats":
		for _, f := range translate.Default().Formats() {
			fmt.Printf("%-13s %s\n", f, translate.Default()[f].Description())
		}
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, cmd, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "sgw.yaml", "config file")
	insecure := fs.Bool("allow-insecure", false, "permit auth.mode=none (every request is trusted)")
	fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authn, err := buildAuth(ctx, cfg, *insecure, log)
	if err != nil {
		return err
	}
	pol, err := policy.Load(cfg.Policy)
	if err != nil {
		return err
	}
	st, err := sqlite.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := registry.New(st, pol)
	srv := &server.Server{
		Reg: reg, Auth: authn, Translators: translate.Default(), Log: log,
		MaxBody: cfg.MaxUploadBytes, MCP: mcp.NewHandler(reg, version),
	}
	hs := &http.Server{
		Addr: cfg.Listen, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Info("skills gateway listening", "addr", cfg.Listen, "auth", cfg.Auth.Mode, "version", version)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func buildAuth(ctx context.Context, cfg *config.Config, insecure bool, log *slog.Logger) (auth.Authenticator, error) {
	switch cfg.Auth.Mode {
	case "oidc":
		return auth.NewOIDC(ctx, cfg.Auth.OIDC)
	case "static":
		log.Warn("static token auth is for development and CI; use oidc in production")
		return auth.NewStatic(cfg.Auth.StaticTable()), nil
	case "none":
		if !insecure {
			return nil, errors.New("auth.mode=none trusts every request; pass -allow-insecure to run it anyway")
		}
		p := cfg.Auth.None
		if p.Subject == "" {
			p.Subject = "anonymous"
		}
		log.Warn("AUTHENTICATION DISABLED: every request runs as " + p.Subject)
		return auth.None{P: p}, nil
	}
	return nil, fmt.Errorf("unknown auth mode %q", cfg.Auth.Mode)
}

func newClient() (*client.Client, error) {
	u := os.Getenv("SGW_URL")
	if u == "" {
		return nil, errors.New("set SGW_URL to the gateway base URL, e.g. http://127.0.0.1:8080")
	}
	return &client.Client{BaseURL: u, Token: os.Getenv("SGW_TOKEN"), HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

// parseRef splits "ns/name[@version]"; version defaults to "latest".
func parseRef(ref string) (ns, name, ver string, err error) {
	ver = "latest"
	orig := ref
	if i := strings.LastIndexByte(ref, '@'); i > 0 {
		ref, ver = ref[:i], ref[i+1:]
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || ver == "" {
		return "", "", "", fmt.Errorf("skill reference must be <namespace>/<name>[@version], got %q", orig)
	}
	return parts[0], parts[1], ver, nil
}

// flagsAfter lets flags follow positional arguments ("sgw fetch a/b -out x").
func flagsAfter(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	ns := fs.String("ns", "", "namespace (required)")
	ver := fs.String("version", "", "semantic version (required)")
	pos, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *ns == "" || *ver == "" {
		return errors.New("usage: sgw publish <skill-dir> -ns <namespace> -version <semver>")
	}
	files, err := readDir(pos[0])
	if err != nil {
		return err
	}
	name := filepath.Base(filepath.Clean(pos[0]))
	c, err := newClient()
	if err != nil {
		return err
	}
	v, err := c.Publish(context.Background(), *ns, name, *ver, files)
	if err != nil {
		return err
	}
	fmt.Printf("published %s/%s@%s (%d files)\ndigest %s\n", v.Namespace, v.Name, v.Version, len(v.Files), v.Digest)
	return nil
}

// readDir loads a skill directory, skipping hidden files and refusing links.
func readDir(dir string) ([]bundle.File, error) {
	var files []bundle.File
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; bundles may only contain regular files", rel)
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, bundle.File{Path: rel, Content: data})
		return nil
	})
	return files, err
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	ns := fs.String("ns", "", "namespace")
	q := fs.String("q", "", "text search")
	tag := fs.String("tag", "", "tag")
	pos, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if *q == "" && len(pos) > 0 {
		*q = strings.Join(pos, " ")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SKILL\tVERSION\tDESCRIPTION")
	cursor := ""
	for {
		page, err := c.List(context.Background(), *ns, *q, *tag, cursor, 200)
		if err != nil {
			return err
		}
		for _, v := range page.Items {
			d := v.Description
			if len(d) > 70 {
				d = d[:67] + "..."
			}
			fmt.Fprintf(tw, "%s/%s\t%s\t%s\n", v.Namespace, v.Name, v.Version, d)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return tw.Flush()
}

func get(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sgw get <ns>/<name>[@version]")
	}
	ns, name, ver, err := parseRef(args[0])
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	v, err := c.Get(context.Background(), ns, name, ver)
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s@%s  [%s]\n%s\npublisher %s at %s\ndigest    %s\n", v.Namespace, v.Name, v.Version, v.Status,
		v.Description, v.Publisher, v.PublishedAt.Format(time.RFC3339), v.Digest)
	for _, f := range v.Files {
		fmt.Printf("  %-40s %8d  sha256:%s\n", f.Path, f.Size, f.SHA256)
	}
	return nil
}

func fetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	format := fs.String("format", "claude", "agent format (see: sgw formats)")
	out := fs.String("out", ".", "project root to write into")
	pos, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: sgw fetch <ns>/<name>[@version] [-format claude] [-out .]")
	}
	ns, name, ver, err := parseRef(pos[0])
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	res, err := syncer.Install(context.Background(), c, translate.Default(), *out, ns, name, ver, []string{*format})
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	fmt.Printf("installed %s/%s@%s (%s) — %d file(s) written\n", ns, name, res.Version, res.Digest, len(res.Written))
	return nil
}

func syncCmd(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	file := fs.String("file", "sgw-sync.yaml", "sync manifest")
	out := fs.String("out", ".", "project root to write into")
	fs.Parse(args)
	m, err := syncer.LoadManifest(*file)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	rep, err := syncer.Sync(context.Background(), c, translate.Default(), *out, m)
	if err != nil {
		return err
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	for _, s := range rep.Skills {
		fmt.Printf("synced %s@%s %s\n", s.Ref, s.Version, s.Digest)
	}
	fmt.Printf("%d file(s) written, %d stale file(s) removed; lock: %s\n", rep.Written, rep.Removed, syncer.LockFile)
	return nil
}

func deprecate(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sgw deprecate <ns>/<name>@<version>")
	}
	ns, name, ver, err := parseRef(args[0])
	if err != nil {
		return err
	}
	if ver == "latest" {
		return errors.New("deprecate needs an explicit version")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	v, err := c.Deprecate(context.Background(), ns, name, ver)
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s@%s is now %s\n", v.Namespace, v.Name, v.Version, v.Status)
	return nil
}

func audit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	since := fs.String("since", "", "RFC 3339 start time")
	fs.Parse(args)
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.Audit(context.Background(), *since, io.Writer(os.Stdout))
}
