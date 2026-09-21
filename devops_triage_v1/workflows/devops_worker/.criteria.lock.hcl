schema_version = 1
adapter "copilot" "devops" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.8"
  version              = "0.5.8"
  resolved_digest      = "sha256:135845cb463f92a7a9e8b7cfe917839a2828b4ba62de6370adde61c6c4161acc"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.8"
    }
  }
}
adapter "shell" "wt" {
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
