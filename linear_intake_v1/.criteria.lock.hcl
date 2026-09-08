schema_version = 1
adapter "copilot" "intake_classifier" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.1.1"
  version              = "0.1.1"
  resolved_digest      = "sha256:2dbbc5e25853eecd9253b4b9d1f626dd73d4883dd44e99cf200f8a06ab7b26bf"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.1.1"
    }
  }
  container_image {
    ref    = "ghcr.io/brokenbots/criteria-adapter-copilot:0.1.1@sha256:6adf134adfecf441b0fcb7a08a35c6a663eaaefbd172f9efcbab6ac17dd6e15e"
    digest = "sha256:6adf134adfecf441b0fcb7a08a35c6a663eaaefbd172f9efcbab6ac17dd6e15e"
  }
}
adapter "copilot" "triage_reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.1.1"
  version              = "0.1.1"
  resolved_digest      = "sha256:2dbbc5e25853eecd9253b4b9d1f626dd73d4883dd44e99cf200f8a06ab7b26bf"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.1.1"
    }
  }
  container_image {
    ref    = "ghcr.io/brokenbots/criteria-adapter-copilot:0.1.1@sha256:6adf134adfecf441b0fcb7a08a35c6a663eaaefbd172f9efcbab6ac17dd6e15e"
    digest = "sha256:6adf134adfecf441b0fcb7a08a35c6a663eaaefbd172f9efcbab6ac17dd6e15e"
  }
}
adapter "shell" "intake" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:2.0.1"
  version              = "2.0.1"
  resolved_digest      = "sha256:08d0c137b22ad7f31b2870d548aa7ebb97c8abd7d1e75d29641dada408994d75"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v2.0.1"
    }
  }
  container_image {
    ref    = "ghcr.io/brokenbots/criteria-adapter-shell:2.0.1@sha256:f07ba445b74fa2f7117601f37e29eb5ae39e39469161e70b5cf990968ac5a3ae"
    digest = "sha256:f07ba445b74fa2f7117601f37e29eb5ae39e39469161e70b5cf990968ac5a3ae"
  }
}
