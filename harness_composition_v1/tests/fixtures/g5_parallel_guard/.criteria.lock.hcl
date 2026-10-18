schema_version = 1
adapter "copilot" "guarded" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.15"
  version              = "0.5.15"
  resolved_digest      = "sha256:827e39ad040a24ad81251b802b1996a00195121ce2c97b7b6ba62d558d581773"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.15"
    }
  }
}