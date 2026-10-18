schema_version = 1
adapter "shell" "pass_in" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.5"
  version              = "0.5.5"
  resolved_digest      = "sha256:41288ce9ce5d3d46841f40b85208c6208c596f36993a898c23002acfa9100a41"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.5"
    }
  }
}