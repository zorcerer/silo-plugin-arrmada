// Command index writes the Silo plugin repository index (index.json) for one
// release: the plugin's manifest, and the release's binaries with their
// SHA-256 checksums. Silo reads it from a repository URL added under Admin,
// Plugins; the release workflow attaches it to each release, so
// releases/latest/download/index.json always names the newest one.
//
//	go run ./cmd/index -version 1.2.3 -dist dist > index.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// defaultRepo is where releases are published; release.yml passes its own
// repository, so the index follows the plugin if it moves.
const defaultRepo = "zorcerer/silo-plugin-arrmada"

// The index shape Silo decodes (internal/plugins/catalog_service.go), written
// with encoding/json as Silo's own catalog tool writes it.
type platformBinary struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}

type catalogPackage struct {
	Manifest     *pluginv1.PluginManifest  `json:"manifest"`
	RepoURL      string                    `json:"repo_url"`
	ChecksumsURL string                    `json:"checksums_url,omitempty"`
	BundleURL    string                    `json:"bundle_url,omitempty"`
	Binaries     map[string]platformBinary `json:"binaries"`
}

type repositoryIndex struct {
	Plugins []catalogPackage `json:"plugins"`
}

var (
	semver   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	repoName = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
)

func main() {
	version := flag.String("version", "", "release version, without the v")
	dist := flag.String("dist", "dist", "folder holding plugin-<os>-<arch>")
	manifestPath := flag.String("manifest", "manifest.json", "the plugin's manifest")
	bundle := flag.Bool("bundle", false, "the release has plugin.sigstore.json")
	repo := flag.String("repo", defaultRepo, "GitHub repository the release is in, owner/name")
	flag.Parse()
	if err := run(os.Stdout, *repo, *version, *dist, *manifestPath, *bundle); err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		os.Exit(1)
	}
}

func run(w io.Writer, repo, version, dist, manifestPath string, bundle bool) error {
	idx, err := build(repo, version, dist, manifestPath, bundle)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

func build(repo, version, dist, manifestPath string, bundle bool) (repositoryIndex, error) {
	if !repoName.MatchString(repo) {
		return repositoryIndex{}, fmt.Errorf("repository %q is not owner/name", repo)
	}
	if !semver.MatchString(version) {
		return repositoryIndex{}, fmt.Errorf("version %q is not 1.2.3", version)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return repositoryIndex{}, err
	}
	var m pluginv1.PluginManifest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &m); err != nil {
		return repositoryIndex{}, fmt.Errorf("decode %s: %w", manifestPath, err)
	}
	m.Version = version
	// Each binary stamps its own checksum when it runs; the index carries
	// one per platform instead.
	m.Checksum = ""

	download := "https://github.com/" + repo + "/releases/download/v" + version + "/"
	pkg := catalogPackage{
		Manifest:     &m,
		RepoURL:      "https://github.com/" + repo,
		ChecksumsURL: download + "checksums.txt",
		Binaries:     map[string]platformBinary{},
	}
	if bundle {
		pkg.BundleURL = download + "plugin.sigstore.json"
	}
	var missing []string
	for _, p := range m.GetSupportedPlatforms() {
		name := "plugin-" + p.GetOs() + "-" + p.GetArch()
		sum, err := sha256File(filepath.Join(dist, name))
		if err != nil {
			missing = append(missing, name)
			continue
		}
		pkg.Binaries[p.GetOs()+"/"+p.GetArch()] = platformBinary{URL: download + name, Checksum: sum}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return repositoryIndex{}, fmt.Errorf("%s has no %s", dist, strings.Join(missing, ", "))
	}
	return repositoryIndex{Plugins: []catalogPackage{pkg}}, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
