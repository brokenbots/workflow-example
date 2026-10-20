schema_version = 1
adapter "copilot" "triage_reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.17"
  version              = "0.5.17"
  resolved_digest      = "sha256:d4400abc734ab4146440e9ab9ac1d22eea11ef38166895718b415f8dc46671ae"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.17"
    }
  }
}
