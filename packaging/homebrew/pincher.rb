# Homebrew formula for pincherMCP.
#
# Pinned to v0.36.0. The SHA256 values below are from the authoritative
# SHA256SUMS file published with that release:
# https://github.com/kwad77/pincher/releases/download/v0.36.0/SHA256SUMS
#
# Usage:
#   brew tap kwad77/pincher https://github.com/kwad77/homebrew-pincher
#   brew install pincher
#
# To host the tap yourself, create a repo named "homebrew-pincher" under
# your GitHub account and drop this file in at Formula/pincher.rb.
#
# On each new release: bump `version`, refetch the release's SHA256SUMS,
# and paste the four new Darwin/Linux (arm64/amd64) hashes into the
# sha256 lines below.
class Pincher < Formula
  desc "Codebase intelligence server for LLM agents (MCP stdio + HTTP REST)"
  homepage "https://github.com/kwad77/pincher"
  version "0.36.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/kwad77/pincher/releases/download/v#{version}/pincher-v#{version}-darwin-arm64.tar.gz"
      sha256 "74c9b32dc6330ffd7b128905a1e407eed5fb4667c20f389b892a35b46b1c18cf"
    end
    on_intel do
      url "https://github.com/kwad77/pincher/releases/download/v#{version}/pincher-v#{version}-darwin-amd64.tar.gz"
      sha256 "07e56b3c86047a1333a571afbb25114f44119afdd344be6006cb4d47b261e725"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/kwad77/pincher/releases/download/v#{version}/pincher-v#{version}-linux-arm64.tar.gz"
      sha256 "91de8ed7a4938922aae2c0de1b3a27a9c6e724fb5e0a54d68e1b6f9b349bf94b"
    end
    on_intel do
      url "https://github.com/kwad77/pincher/releases/download/v#{version}/pincher-v#{version}-linux-amd64.tar.gz"
      sha256 "2436a77e6ba2d79be192d93dda86c9ef3604629acb38d8af266aba32e5791987"
    end
  end

  def install
    # Archives contain one file: pincher-v<version>-<os>-<arch>[.exe]
    bin_src = Dir["pincher-*"].first
    bin.install bin_src => "pincher"
  end

  test do
    assert_match "pincherMCP", shell_output("#{bin}/pincher --version")
  end

  service do
    run [opt_bin/"pincher", "--http", ":8080"]
    keep_alive true
    log_path var/"log/pincher.log"
    error_log_path var/"log/pincher.err.log"
    environment_variables PINCHER_HTTP_ADDR: ":8080"
  end
end
