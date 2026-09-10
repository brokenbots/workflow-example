schema_version = 1
adapter "copilot" "investigator" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.6"
  version              = "0.5.6"
  resolved_digest      = "sha256:279e691dc071fe9c0aedc6d07aedd6c555454242d39d2b8fd4def02d49950c76"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.6"
    }
  }
}
adapter "copilot" "supervisor" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.6"
  version              = "0.5.6"
  resolved_digest      = "sha256:279e691dc071fe9c0aedc6d07aedd6c555454242d39d2b8fd4def02d49950c76"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.6"
    }
  }
}
adapter "shell" "repo" {
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
