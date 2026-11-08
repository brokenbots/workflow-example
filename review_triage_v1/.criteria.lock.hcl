schema_version = 1
adapter "copilot" "triage_reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.19"
  version              = "0.5.19"
  resolved_digest      = "sha256:7ea26c0b0ad5bcdf681f397265f56024105f450dc60ffdc5f3c844edcf08568c"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.19"
    }
  }
}
