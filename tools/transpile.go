package tools

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// transpileTS converts TypeScript source to JavaScript using esbuild's pure-Go
// Transform API. Target is ES2020 to match goja's capability. Returns the
// transpiled JS or an error with diagnostic details.
func transpileTS(source string) (string, error) {
	result := api.Transform(source, api.TransformOptions{
		Loader: api.LoaderTS,
		Target: api.ES2020,
	})

	if len(result.Errors) > 0 {
		var msgs []string
		for _, e := range result.Errors {
			if e.Location != nil {
				msgs = append(msgs, fmt.Sprintf("line %d: %s", e.Location.Line, e.Text))
			} else {
				msgs = append(msgs, e.Text)
			}
		}
		return "", fmt.Errorf("TypeScript transpilation failed: %s", strings.Join(msgs, "; "))
	}

	return string(result.Code), nil
}

// ValidateTypeScriptSource transpiles TypeScript to JavaScript via esbuild,
// then validates the resulting JS in goja. Returns the transpiled JS on
// success so callers can use it directly (e.g. for test execution).
func ValidateTypeScriptSource(source string) (string, error) {
	js, err := transpileTS(source)
	if err != nil {
		return "", err
	}

	if err := validateSkillSource(js); err != nil {
		return "", fmt.Errorf("transpiled JS validation failed: %w", err)
	}

	return js, nil
}

