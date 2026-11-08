// Package setup manages verified native runtime and model installations.
package setup

// RuntimeVersion and ModelRevision are the validated inference pins.
const (
	RuntimeVersion = "0.3.0"
	ModelRevision  = "f667ed73aee57d40cc39428eb768b4fd87a0a29e"
)

type artifact struct {
	name, url, sha256 string
	size              int64
}

type catalog struct {
	assets []artifact
	prefix string
}

func pinnedCatalog() catalog {
	return catalog{
		prefix: "nemo-speech-0.3.0-macos-aarch64-metal",
		assets: []artifact{
			{"runtime.tar.gz", "https://github.com/NVIDIA/NeMo-Speech.cpp/releases/download/v0.3.0/nemo-speech-0.3.0-macos-aarch64-metal.tar.gz", "2b972ed8b5dba07492212acf8cb75cbf34ecb141198ffa1997febe84b7c4d9ed", 7239335},
			{"model.gguf", "https://huggingface.co/nvidia/Nemotron-3-Diarization/resolve/" + ModelRevision + "/Nemotron-3-Diarization.q8_0.gguf", "08456d9e22cd9a323c0364d98375f3746d6e68507ebb705cd46438c534c7a3a1", 107012128},
			{"MODEL-CARD.md", "https://huggingface.co/nvidia/Nemotron-3-Diarization/resolve/" + ModelRevision + "/README.md", "5fe88ca4020d2ee134f806a0a02c52c19855b73047bc2e6f25b9171e1abeebfd", 43239},
			{"MODEL-LICENSE", "https://raw.githubusercontent.com/OpenMDW/OpenMDW/9067ba54b2f84bc9e15a05734f570ec88e9f445d/1.1/LICENSE.OpenMDW-1.1", "2ab44b68365473c112f5092211a38f231cb23e50de68b75a13369adbd76a74df", 2619},
		},
	}
}
