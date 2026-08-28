schema_version = 1
adapter "copilot" "developer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4"
  version              = "0.5.4"
  resolved_digest      = "sha256:b3bea8a4f03e3caccdeea6fca9aab6f9422528d855603c1b4c0f9c64b7b76e9e"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.4"
    }
  }
}
adapter "copilot" "reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4"
  version              = "0.5.4"
  resolved_digest      = "sha256:b3bea8a4f03e3caccdeea6fca9aab6f9422528d855603c1b4c0f9c64b7b76e9e"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.4"
    }
  }
}
adapter "noop" "default" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.2"
  version              = "0.5.2"
  resolved_digest      = "sha256:00ab4151baacba3b14e89cdd5c99e0c1924e23198d0ff91387f995021d75ce4f"
  source_url           = "https://github.com/brokenbots/criteria-adapter-noop"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-noop/.github/workflows/publish.yml@refs/tags/v0.5.2"
    }
  }
}
adapter "shell" "sh" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.2"
  version              = "0.5.2"
  resolved_digest      = "sha256:38f23c92a11548ce57c54e9312c567558d3ad017fd632adfba305b058988703d"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.2"
    }
  }
}
