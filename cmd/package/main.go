// The release packager is a development tool. Installed users need only binaries.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type target struct {
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Version string            `json:"version"`
	Targets map[string]target `json:"targets"`
}

func main() {
	version := flag.String("version", "", "release version, e.g. 0.2.0")
	owner := flag.String("owner", "codeasy-digix", "GitHub owner")
	flag.Parse()
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(*version) || !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(*owner) {
		fail(fmt.Errorf("supply -version x.y.z and a valid GitHub owner"))
	}
	data, err := os.ReadFile("packaging/codex-runtime.json")
	check(err)
	var upstream manifest
	check(json.Unmarshal(data, &upstream))
	check(os.MkdirAll("dist", 0755))
	checksums := make(map[string]string)
	var sums strings.Builder
	for _, platform := range []string{"darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64"} {
		parts := strings.Split(platform, "_")
		root, err := os.MkdirTemp("", "codex-accounts-package-")
		check(err)
		binary := filepath.Join(root, "codex-accounts")
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -buildid= -X main.version="+*version+" -X main.codexVersion="+upstream.Version, "-o", binary, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		check(cmd.Run())
		name := "codex-accounts_" + *version + "_" + platform + ".tar.gz"
		output, err := os.Create(filepath.Join("dist", name))
		check(err)
		hash := sha256.New()
		gz := gzip.NewWriter(io.MultiWriter(output, hash))
		tw := tar.NewWriter(gz)
		for _, entry := range []struct{ source, name string }{
			{binary, "codex-accounts"}, {"LICENSE", "LICENSE"}, {"README.md", "README.md"}, {"THIRD_PARTY_NOTICES.md", "THIRD_PARTY_NOTICES.md"},
			{"third_party/GO_LICENSE", "third_party/GO_LICENSE"}, {"third_party/CODEX_LICENSE", "third_party/CODEX_LICENSE"}, {"third_party/CODEX_NOTICE", "third_party/CODEX_NOTICE"},
		} {
			contents, err := os.ReadFile(entry.source)
			check(err)
			mode := int64(0644)
			if entry.name == "codex-accounts" {
				mode = 0755
			}
			check(tw.WriteHeader(&tar.Header{Name: entry.name, Mode: mode, Size: int64(len(contents))}))
			_, err = tw.Write(contents)
			check(err)
		}
		check(tw.Close())
		check(gz.Close())
		check(output.Close())
		checksums[platform] = hex.EncodeToString(hash.Sum(nil))
		fmt.Fprintf(&sums, "%s  %s\n", checksums[platform], name)
		check(os.RemoveAll(root))
		fmt.Println("Built", name)
	}
	check(os.WriteFile("dist/SHA256SUMS", []byte(sums.String()), 0644))
	check(os.WriteFile("dist/codex-accounts.rb", []byte(formula(*version, *owner, upstream, checksums)), 0644))
}

func formula(version, owner string, upstream manifest, hashes map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `class CodexAccounts < Formula
  desc "Per-terminal Codex accounts with shared local conversations"
  homepage "https://github.com/%s/codex-accounts"
  version "%s"
  license "MIT"

  depends_on "tmux"

`, owner, version)
	for _, osName := range []string{"macos", "linux"} {
		goos := osName
		if osName == "macos" {
			goos = "darwin"
		}
		fmt.Fprintf(&b, "  on_%s do\n", osName)
		for _, arch := range []string{"arm64", "amd64"} {
			brewarch := "arm"
			if arch == "amd64" {
				brewarch = "intel"
			}
			platform := goos + "_" + arch
			u := upstream.Targets[platform]
			fmt.Fprintf(&b, `    on_%s do
      url "https://github.com/%s/codex-accounts/releases/download/v%s/codex-accounts_%s_%s.tar.gz"
      sha256 "%s"

      resource "codex-runtime" do
        url "https://github.com/openai/codex/releases/download/rust-v%s/codex-package-%s.tar.gz"
        sha256 "%s"
      end
    end
`, brewarch, owner, version, version, platform, hashes[platform], upstream.Version, u.Target, u.SHA256)
		}
		b.WriteString("  end\n\n")
	}
	fmt.Fprintf(&b, `  def install
    bin.install "codex-accounts"
    resource("codex-runtime").stage do
      (libexec/"codex").install Dir["*"]
    end
    (pkgshare/"licenses").install Dir["third_party/*"]
  end

  def caveats
    <<~EOS
      Ready to use: codex-accounts --account ryu

      For per-terminal 'codex account NAME', add ONE line to your shell profile:
        Zsh (~/.zshrc):  eval "$(codex-accounts shell-init zsh)"
        Bash (~/.bashrc): eval "$(codex-accounts shell-init bash)"

      Run: codex account NAME; codex account NAME default; codex account default
      Resume quota-interrupted conversations: codex continue
      Your existing conversations and account credentials are retained on uninstall.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/codex-accounts --version")
    assert_match "codex-cli %s", shell_output("#{bin}/codex-accounts run --version")
    assert_match "codex_account()", shell_output("#{bin}/codex-accounts shell-init zsh")
    ENV["HOME"] = testpath.to_s
    %%w[
      CODEX_ACCOUNT CODEX_HOME CODEX_SQLITE_HOME CODEX_SHARED_HOME CODEX_ACCOUNTS_DIR CODEX_ACCOUNTS_RUNTIME
    ].each do |key|
      ENV.delete(key)
    end
    assert_match "No registered accounts", shell_output("#{bin}/codex-accounts account --list")
    assert_match "not signed in", shell_output("#{bin}/codex-accounts account")
    assert_match "No quota-interrupted", shell_output("#{bin}/codex-accounts continue --list")
  end
end
`, upstream.Version)
	return b.String()
}

func check(err error) {
	if err != nil {
		fail(err)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
