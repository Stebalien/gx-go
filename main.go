package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	cli "github.com/codegangsta/cli"
	rw "github.com/whyrusleeping/gx-go/rewrite"
	gx "github.com/whyrusleeping/gx/gxutil"
	. "github.com/whyrusleeping/stump"
)

var vendorDir = filepath.Join("vendor", "gx", "ipfs")

var cwd string

// for go packages, extra info
type GoInfo struct {
	DvcsImport string `json:"dvcsimport,omitempty"`

	// GoVersion sets a compiler version requirement, users will be warned if installing
	// a package using an unsupported compiler
	GoVersion string `json:"goversion,omitempty"`
}

type Package struct {
	gx.PackageBase

	Gx GoInfo `json:"gx,omitempty"`
}

func LoadPackageFile(name string) (*Package, error) {
	fi, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	var pkg Package
	err = json.NewDecoder(fi).Decode(&pkg)
	if err != nil {
		return nil, err
	}

	return &pkg, nil
}

func main() {
	app := cli.NewApp()
	app.Name = "gx-go"
	app.Author = "whyrusleeping"
	app.Usage = "gx extensions for golang"
	app.Version = "1.1.0"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "turn on verbose output",
		},
	}
	app.Before = func(c *cli.Context) error {
		Verbose = c.Bool("verbose")
		return nil
	}

	mcwd, err := os.Getwd()
	if err != nil {
		Fatal("failed to get cwd:", err)
	}
	lcwd, err := filepath.EvalSymlinks(mcwd)
	if err != nil {
		Fatal("failed to resolve symlinks of cdw:", err)
	}
	cwd = lcwd

	app.Commands = []cli.Command{
		DepMapCommand,
		HookCommand,
		ImportCommand,
		PathCommand,
		RewriteCommand,
		UpdateCommand,
		DvcsDepsCommand,
	}

	if err := app.Run(os.Args); err != nil {
		Fatal(err)
	}
}

var DepMapCommand = cli.Command{
	Name:  "dep-map",
	Usage: "prints out a json dep map for usage by 'import --map'",
	Action: func(c *cli.Context) error {
		pkg, err := LoadPackageFile(gx.PkgFileName)
		if err != nil {
			return err
		}

		m := make(map[string]string)
		err = buildMap(pkg, m)
		if err != nil {
			return err
		}

		out, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}

		os.Stdout.Write(out)
		return nil
	},
}

var HookCommand = cli.Command{
	Name:  "hook",
	Usage: "go specific hooks to be called by the gx tool",
	Subcommands: []cli.Command{
		postImportCommand,
		reqCheckCommand,
		installLocHookCommand,
		postInitHookCommand,
		postUpdateHookCommand,
		postInstallHookCommand,
	},
	Action: func(c *cli.Context) error { return nil },
}

var ImportCommand = cli.Command{
	Name:  "import",
	Usage: "import a go package and all its depencies into gx",
	Description: `imports a given go package and all of its dependencies into gx
producing a package.json for each, and outputting a package hash
for each.`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "rewrite",
			Usage: "rewrite import paths to use vendored packages",
		},
		cli.BoolFlag{
			Name:  "yesall",
			Usage: "assume defaults for all options",
		},
		cli.BoolFlag{
			Name:  "tmpdir",
			Usage: "create and use a temporary directory for the GOPATH",
		},
		cli.StringFlag{
			Name:  "map",
			Usage: "json document mapping imports to prexisting hashes",
		},
	},
	Action: func(c *cli.Context) error {
		var mapping map[string]string
		preset := c.String("map")
		if preset != "" {
			err := loadMap(&mapping, preset)
			if err != nil {
				return err
			}
		}

		var gopath string
		if c.Bool("tmpdir") {
			dir, err := ioutil.TempDir("", "gx-go-import")
			if err != nil {
				return fmt.Errorf("creating temp dir: %s", err)
			}
			err = os.Setenv("GOPATH", dir)
			if err != nil {
				return fmt.Errorf("setting GOPATH: %s", err)
			}
			Log("setting GOPATH to", dir)

			gopath = dir
		} else {
			gp, err := getGoPath()
			if err != nil {
				return fmt.Errorf("couldnt determine gopath: %s", err)
			}

			gopath = gp
		}

		importer, err := NewImporter(c.Bool("rewrite"), gopath, mapping)
		if err != nil {
			return err
		}

		importer.yesall = c.Bool("yesall")

		if !c.Args().Present() {
			return fmt.Errorf("must specify a package name")
		}

		pkg := c.Args().First()
		Log("vendoring package %s", pkg)

		_, err = importer.GxPublishGoPackage(pkg)
		if err != nil {
			return err
		}

		return nil
	},
}

var UpdateCommand = cli.Command{
	Name:      "update",
	Usage:     "update a packages imports to a new path",
	ArgsUsage: "[old import] [new import]",
	Action: func(c *cli.Context) error {
		if len(c.Args()) < 2 {
			return fmt.Errorf("must specify current and new import names")
		}

		oldimp := c.Args()[0]
		newimp := c.Args()[1]

		err := doUpdate(cwd, oldimp, newimp)
		if err != nil {
			return err
		}

		return nil
	},
}

var RewriteCommand = cli.Command{
	Name:  "rewrite",
	Usage: "temporary hack to evade causality",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "undo",
			Usage: "rewrite import paths back to dvcs",
		},
		cli.BoolFlag{
			Name:  "dry-run",
			Usage: "print out mapping without touching files",
		},
		cli.StringFlag{
			Name:  "pkgdir",
			Usage: "alternative location of the package directory",
		},
	},
	Action: func(c *cli.Context) error {
		pkg, err := LoadPackageFile(gx.PkgFileName)
		if err != nil {
			return err
		}

		pkgdir := filepath.Join(cwd, vendorDir)
		if pdopt := c.String("pkgdir"); pdopt != "" {
			pkgdir = pdopt
		}

		VLog("  - building rewrite mapping")
		mapping := make(map[string]string)
		if !c.Args().Present() {
			err = buildRewriteMapping(pkg, pkgdir, mapping, c.Bool("undo"))
			if err != nil {
				return fmt.Errorf("build of rewrite mapping failed:\n%s", err)
			}
		} else {
			for _, arg := range c.Args() {
				dep := pkg.FindDep(arg)
				if dep == nil {
					return fmt.Errorf("%s not found", arg)
				}

				pkg, err := loadDep(dep, pkgdir)
				if err != nil {
					return err
				}

				addRewriteForDep(dep, pkg, mapping, c.Bool("undo"))
			}
		}
		VLog("  - rewrite mapping complete")

		if c.Bool("dry-run") {
			tabPrintSortedMap(nil, mapping)
			return nil
		}

		err = doRewrite(pkg, cwd, mapping)
		if err != nil {
			return err
		}

		return nil
	},
}

var DvcsDepsCommand = cli.Command{
	Name:  "dvcs-deps",
	Usage: "display dvcs deps that arent tracked in gx",
	Action: func(c *cli.Context) error {
		i, err := NewImporter(false, os.Getenv("GOPATH"), nil)
		if err != nil {
			return err
		}

		relp, err := getImportPath(cwd)
		if err != nil {
			return err
		}

		deps, err := i.DepsToVendorForPackage(relp)
		if err != nil {
			return err
		}

		for _, d := range deps {
			fmt.Println(d)
		}

		return nil
	},
}

func getImportPath(pkgpath string) (string, error) {
	gopath, err := getGoPath()
	if err != nil {
		return "", fmt.Errorf("GOPATH not set, cannot derive import path")
	}

	srcdir := path.Join(gopath, "src")
	srcdir += "/"

	if !strings.HasPrefix(cwd, srcdir) {
		return "", fmt.Errorf("package not within GOPATH/src")
	}

	rel := cwd[len(srcdir):]
	return rel, nil
}

var PathCommand = cli.Command{
	Name:  "path",
	Usage: "prints the import path of the current package within GOPATH",
	Action: func(c *cli.Context) error {
		rel, err := getImportPath(cwd)
		if err != nil {
			return err
		}

		fmt.Println(rel)
		return nil
	},
}

func prompt(text, def string) (string, error) {
	scan := bufio.NewScanner(os.Stdin)
	fmt.Printf("%s (default: '%s') ", text, def)
	for scan.Scan() {
		if scan.Text() != "" {
			return scan.Text(), nil
		}
		return def, nil
	}

	return "", scan.Err()
}

func yesNoPrompt(prompt string, def bool) bool {
	opts := "[y/N]"
	if def {
		opts = "[Y/n]"
	}

	fmt.Printf("%s %s ", prompt, opts)
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		val := strings.ToLower(scan.Text())
		switch val {
		case "":
			return def
		case "y":
			return true
		case "n":
			return false
		default:
			fmt.Println("please type 'y' or 'n'")
		}
	}

	panic("unexpected termination of stdin")
}

var postImportCommand = cli.Command{
	Name:  "post-import",
	Usage: "hook called after importing a new go package",
	Action: func(c *cli.Context) error {
		if !c.Args().Present() {
			Fatal("no package specified")
		}
		dephash := c.Args().First()

		pkg, err := LoadPackageFile(gx.PkgFileName)
		if err != nil {
			return err
		}

		err = postImportHook(pkg, dephash)
		if err != nil {
			return err
		}

		return nil
	},
}

var reqCheckCommand = cli.Command{
	Name:  "req-check",
	Usage: "hook called to check if requirements of a package are met",
	Action: func(c *cli.Context) error {
		if !c.Args().Present() {
			Fatal("no package specified")
		}
		pkgpath := c.Args().First()

		err := reqCheckHook(pkgpath)
		if err != nil {
			return err
		}

		return nil
	},
}

var postInitHookCommand = cli.Command{
	Name:  "post-init",
	Usage: "hook called to perform go specific package initialization",
	Action: func(c *cli.Context) error {
		var dir string
		if c.Args().Present() {
			dir = c.Args().First()
		} else {
			dir = cwd
		}

		pkgpath := filepath.Join(dir, gx.PkgFileName)
		pkg, err := LoadPackageFile(pkgpath)
		if err != nil {
			return err
		}

		imp, _ := packagesGoImport(dir)

		if imp != "" {
			pkg.Gx.DvcsImport = imp
		}

		err = gx.SavePackageFile(pkg, pkgpath)
		if err != nil {
			return err
		}

		return nil
	},
}

var postInstallHookCommand = cli.Command{
	Name:  "post-install",
	Usage: "post install hook for newly installed go packages",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "global",
			Usage: "specifies whether or not the install was global",
		},
	},
	Action: func(c *cli.Context) error {
		if !c.Args().Present() {
			return fmt.Errorf("must specify path to newly installed package")
		}
		npkg := c.Args().First()
		// update sub-package refs here
		// ex:
		// if this package is 'github.com/X/Y' replace all imports
		// matching 'github.com/X/Y*' with 'gx/<hash>/name*'

		var pkg Package
		err := gx.FindPackageInDir(&pkg, npkg)
		if err != nil {
			return fmt.Errorf("find package failed: %s", err)
		}

		dir := filepath.Join(npkg, pkg.Name)

		// build rewrite mapping from parent package if
		// this call is made on one in the vendor directory
		var reldir string
		if strings.Contains(npkg, "vendor/gx/ipfs") {
			reldir = strings.Split(npkg, "vendor/gx/ipfs")[0]
			reldir = filepath.Join(reldir, "vendor", "gx", "ipfs")
		} else {
			reldir = dir
		}

		mapping := make(map[string]string)
		err = buildRewriteMapping(&pkg, reldir, mapping, false)
		if err != nil {
			return fmt.Errorf("building rewrite mapping failed: %s", err)
		}

		hash := filepath.Base(npkg)
		newimp := "gx/ipfs/" + hash + "/" + pkg.Name
		mapping[pkg.Gx.DvcsImport] = newimp

		err = doRewrite(&pkg, dir, mapping)
		if err != nil {
			return fmt.Errorf("rewrite failed: %s", err)
		}

		return nil
	},
}

func doRewrite(pkg *Package, cwd string, mapping map[string]string) error {
	rwm := func(in string) string {
		m, ok := mapping[in]
		if ok {
			return m
		}

		for k, v := range mapping {
			if strings.HasPrefix(in, k+"/") {
				nmapping := strings.Replace(in, k, v, 1)
				mapping[in] = nmapping
				return nmapping
			}
		}

		mapping[in] = in
		return in
	}

	filter := func(s string) bool {
		return strings.HasSuffix(s, ".go")
	}

	VLog("  - rewriting imports")
	err := rw.RewriteImports(cwd, rwm, filter)
	if err != nil {
		return err
	}
	VLog("  - finished!")

	return nil
}

var installLocHookCommand = cli.Command{
	Name:  "install-path",
	Usage: "prints out install path",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "global",
			Usage: "print global install directory",
		},
	},
	Action: func(c *cli.Context) error {
		if c.Bool("global") {
			gpath, err := getGoPath()
			if err != nil {
				return fmt.Errorf("GOPATH not set")
			}
			fmt.Println(filepath.Join(gpath, "src"))
			return nil
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("install-path cwd: %s", err)
			}

			fmt.Println(filepath.Join(cwd, "vendor"))
			return nil
		}
	},
}

var postUpdateHookCommand = cli.Command{
	Name:  "post-update",
	Usage: "rewrite go package imports to new versions",
	Action: func(c *cli.Context) error {
		if len(c.Args()) < 2 {
			Fatal("must specify two arguments")
		}
		before := "gx/ipfs/" + c.Args()[0]
		after := "gx/ipfs/" + c.Args()[1]
		err := doUpdate(cwd, before, after)
		if err != nil {
			return err
		}

		return nil
	},
}

func packagesGoImport(p string) (string, error) {
	gopath, err := getGoPath()
	if err != nil {
		return "", err
	}

	srcdir := path.Join(gopath, "src")
	srcdir += "/"

	if !strings.HasPrefix(p, srcdir) {
		return "", fmt.Errorf("package not within GOPATH/src")
	}

	return p[len(srcdir):], nil
}

func postImportHook(pkg *Package, npkgHash string) error {
	var npkg Package
	err := gx.LoadPackage(&npkg, "go", npkgHash)
	if err != nil {
		return err
	}

	if npkg.Gx.DvcsImport != "" {
		q := fmt.Sprintf("update imports of %s to the newly imported package?", npkg.Gx.DvcsImport)
		if yesNoPrompt(q, false) {
			nimp := fmt.Sprintf("gx/ipfs/%s/%s", npkgHash, npkg.Name)
			err := doUpdate(cwd, npkg.Gx.DvcsImport, nimp)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func reqCheckHook(pkgpath string) error {
	var npkg Package
	pkgfile := filepath.Join(pkgpath, gx.PkgFileName)
	err := gx.LoadPackageFile(&npkg, pkgfile)
	if err != nil {
		return err
	}

	if npkg.Gx.GoVersion != "" {
		out, err := exec.Command("go", "version").CombinedOutput()
		if err != nil {
			return fmt.Errorf("no go compiler installed")
		}

		parts := strings.Split(string(out), " ")
		if len(parts) < 4 {
			return fmt.Errorf("unrecognized output from go compiler")
		}

		havevers := parts[2][2:]

		reqvers := npkg.Gx.GoVersion

		badreq, err := versionComp(havevers, reqvers)
		if err != nil {
			return err
		}
		if badreq {
			return fmt.Errorf("package '%s' requires at least go version %s, you have %s installed.", npkg.Name, reqvers, havevers)
		}

		gxgocompvers := runtime.Version()
		if strings.HasPrefix(gxgocompvers, "go") {
			badreq, err := versionComp(gxgocompvers[2:], reqvers)
			if err != nil {
				return err
			}
			if badreq {
				return fmt.Errorf("package '%s' requires at least go version %s.\nhowever, your gx-go binary was compiled with %s.\nPlease update gx-go (or recompile with your current go compiler)", npkg.Name, reqvers, gxgocompvers)
			}
		} else {
			Log("gx-go was compiled with an unrecognized version of go. (%s)", gxgocompvers)
			Log("If you encounter any strange issues during its usage, try rebuilding gx-go with go %s or higher", reqvers)
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func versionComp(have, req string) (bool, error) {
	hp := strings.Split(have, ".")
	rp := strings.Split(req, ".")

	l := min(len(hp), len(rp))
	hp = hp[:l]
	rp = rp[:l]
	for i, v := range hp {
		hv, err := strconv.Atoi(v)
		if err != nil {
			return false, err
		}

		rv, err := strconv.Atoi(rp[i])
		if err != nil {
			return false, err
		}

		if hv < rv {
			return true, nil
		} else if hv > rv {
			return false, nil
		}
	}
	return false, nil
}

func globalPath() string {
	gp, _ := getGoPath()
	return filepath.Join(gp, "src", "gx", "ipfs")
}

func loadDep(dep *gx.Dependency, pkgdir string) (*Package, error) {
	var cpkg Package
	pdir := filepath.Join(pkgdir, dep.Hash)
	VLog("  - fetching dep: %s (%s)", dep.Name, dep.Hash)
	err := gx.FindPackageInDir(&cpkg, pdir)
	if err != nil {
		// try global
		p := filepath.Join(globalPath(), dep.Hash)
		VLog("  - checking in global namespace (%s)", p)
		gerr := gx.FindPackageInDir(&cpkg, p)
		if gerr != nil {
			return nil, fmt.Errorf("failed to find package: %s", gerr)
		}
	}

	return &cpkg, nil
}

func addRewriteForDep(dep *gx.Dependency, pkg *Package, m map[string]string, undo bool) {
	if pkg.Gx.DvcsImport != "" {
		from := pkg.Gx.DvcsImport
		to := "gx/ipfs/" + dep.Hash + "/" + pkg.Name
		if undo {
			from, to = to, from
		}
		m[from] = to
	}
}

func buildRewriteMapping(pkg *Package, pkgdir string, m map[string]string, undo bool) error {
	for _, dep := range pkg.Dependencies {
		cpkg, err := loadDep(dep, pkgdir)
		if err != nil {
			return fmt.Errorf("loading dep %q of %q: %s", dep.Name, pkg.Name, err)
		}

		addRewriteForDep(dep, cpkg, m, undo)

		// recurse!
		err = buildRewriteMapping(cpkg, pkgdir, m, undo)
		if err != nil {
			return err
		}
	}

	return nil
}

func buildMap(pkg *Package, m map[string]string) error {
	for _, dep := range pkg.Dependencies {
		var ch Package
		err := gx.FindPackageInDir(&ch, filepath.Join(vendorDir, dep.Hash))
		if err != nil {
			return err
		}

		if ch.Gx.DvcsImport != "" {
			e, ok := m[ch.Gx.DvcsImport]
			if ok {
				if e != dep.Hash {
					Log("have two dep packages with same import path: ", ch.Gx.DvcsImport)
					Log("  - ", e)
					Log("  - ", dep.Hash)
				}
				continue
			}
			m[ch.Gx.DvcsImport] = dep.Hash
		}

		err = buildMap(&ch, m)
		if err != nil {
			return err
		}
	}
	return nil
}

func loadMap(i interface{}, file string) error {
	fi, err := os.Open(file)
	if err != nil {
		return err
	}
	defer fi.Close()

	return json.NewDecoder(fi).Decode(i)
}

func tabPrintSortedMap(headers []string, m map[string]string) {
	var names []string
	for k, _ := range m {
		names = append(names, k)
	}

	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 12, 4, 1, ' ', 0)
	if headers != nil {
		fmt.Fprintf(w, "%s\t%s\n", headers[0], headers[1])
	}

	for _, n := range names {
		fmt.Fprintf(w, "%s\t%s\n", n, m[n])
	}
	w.Flush()
}

func getGoPath() (string, error) {
	gp := os.Getenv("GOPATH")
	if gp == "" {
		return "", fmt.Errorf("GOPATH not set")
	}

	return filepath.SplitList(gp)[0], nil
}
