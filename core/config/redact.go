package config

import (
	"bytes"
	"os"
	"strings"

	"github.com/AnnaofArendelle/codespace-ssh-gateway/core/secret"

	"gopkg.in/yaml.v3"
)

// sensitiveKey reports whether a config key holds a credential itself, as
// opposed to naming a file that holds one or switching a feature on.
func sensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, suffix := range []string{"_file", "_path", "_auth", "_policy", "_source", "_type"} {
		if strings.HasSuffix(k, suffix) {
			return false
		}
	}
	for _, needle := range []string{"token", "password", "secret", "passphrase", "credential", "private_key"} {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}

// RedactedFile returns the config file with every credential replaced, suitable
// for printing or pasting into a bug report. Comments and layout are preserved.
func RedactedFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	redactNode(&doc, false)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// redactNode walks the tree; parentSensitive covers nested values under a
// sensitive key (for example a list of tokens).
func redactNode(n *yaml.Node, parentSensitive bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			redactNode(c, parentSensitive)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			redactNode(val, parentSensitive || sensitiveKey(key.Value))
		}
	case yaml.ScalarNode:
		// Only strings can be credentials; booleans and numbers are settings.
		if parentSensitive && n.Value != "" && (n.Tag == "" || n.Tag == "!!str") {
			n.SetString(secret.Redacted)
		}
	}
}
