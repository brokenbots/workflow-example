schema_version = 1
adapter "copilot" "triage_reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.18"
  version              = "0.5.18"
  resolved_digest      = "sha256:294cf2e7d04d8d64fef774a8e1d652509fa1e1580ab6f892d1c66b334f027b7b"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.18"
    }
  }
}
