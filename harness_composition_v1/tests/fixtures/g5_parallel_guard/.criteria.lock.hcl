schema_version = 1
adapter "copilot" "guarded" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.20"
  version              = "0.5.20"
  resolved_digest      = "sha256:e47d332784ae50f000b46730403cb747fa326d4bbfa032c21380dafb2246e263"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.20"
    }
  }
}
