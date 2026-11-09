schema_version = 1
adapter "copilot" "developer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.21"
  version              = "0.5.21"
  resolved_digest      = "sha256:acd4ed017668ccea354d78d8e88c31ab5c2f35d64f5d3fbd13519dc2a2007a5d"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.21"
    }
  }
}
adapter "copilot" "reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.21"
  version              = "0.5.21"
  resolved_digest      = "sha256:acd4ed017668ccea354d78d8e88c31ab5c2f35d64f5d3fbd13519dc2a2007a5d"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.21"
    }
  }
}
adapter "shell" "sh" {
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
