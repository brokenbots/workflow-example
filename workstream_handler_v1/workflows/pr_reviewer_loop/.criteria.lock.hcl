schema_version = 1
adapter "copilot" "pr_reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.6"
  version              = "0.5.5"
  resolved_digest      = "sha256:aa02ce1e187d783195930f2fbeb011114b9e33cc5556e094b5075203ff6e9699"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.5"
    }
  }
}
adapter "shell" "reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
  version              = "0.5.3"
  resolved_digest      = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.3"
    }
  }
}
