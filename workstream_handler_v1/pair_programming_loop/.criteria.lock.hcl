schema_version = 1
adapter "claude-agent" "reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-claude-agent:0.5.5"
  version              = "0.5.5"
  resolved_digest      = "sha256:0d978bfcab56da1c5a210f7ecec6f3840a66f94eb1ead9152854c5a7f78a4172"
  source_url           = "https://github.com/brokenbots/criteria-typescript-adapter-claude-agent"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-typescript-adapter-claude-agent/.github/workflows/publish.yml@refs/tags/v0.5.5"
    }
  }
}
adapter "copilot" "developer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.1"
  version              = "0.5.1"
  resolved_digest      = "sha256:ad04ab8d4829a3f1f65104ca7261bac4347927a9d026a9ae6d31245ba150c945"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
}
adapter "noop" "default" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.1"
  version              = "0.5.1"
  resolved_digest      = "sha256:690554ed032c238834de83f8105204b8a2e92bd6a4cc281a99bbec3ba2c0983f"
  source_url           = "https://github.com/brokenbots/criteria-adapter-noop"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-noop/.github/workflows/publish.yml@refs/tags/v0.5.1"
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
