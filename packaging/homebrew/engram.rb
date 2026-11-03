# Homebrew formula for Engram. Publish it in a tap repository named
# homebrew-tap (github.com/zakeripouya/homebrew-tap), then:
#   brew install zakeripouya/tap/engram
# Update url and sha256 for each release (sha256 of the source tarball).
class Engram < Formula
  desc "Shared, brain-inspired memory for AI agents (Engram Context Protocol)"
  homepage "https://github.com/zakeripouya/EngramContextProtocol"
  url "https://github.com/zakeripouya/EngramContextProtocol/archive/refs/tags/v0.13.0.tar.gz"
  sha256 "REPLACE_WITH_SOURCE_TARBALL_SHA256"
  license "Apache-2.0"
  head "https://github.com/zakeripouya/EngramContextProtocol.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "1"
    ldflags = "-s -w -X github.com/zakeripouya/EngramContextProtocol/internal/server.Version=#{version}"
    system "go", "build", *std_go_args(ldflags:), "./cmd/engram"
  end

  def caveats
    <<~EOS
      Set up models, agents and the nightly cycle with:
        engram init
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/engram version")
    ENV["ENGRAM_DB"] = testpath/"engram.db"
    ENV["ENGRAM_EMBED"] = "off"
    system bin/"engram", "pair", "test", "--read", "me", "--write", "me"
    assert_match "test", shell_output("#{bin}/engram clients")
  end
end
