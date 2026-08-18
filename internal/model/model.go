// Package model defines provider and model metadata shared by configuration,
// transport adapters, and the OCR pipeline.
package model

import "strings"

// API identifies the wire protocol used by a model endpoint.
type API string

const (
	APIOpenAIChatCompletions API = "openai-chat-completions"
	APIOpenAIResponses       API = "openai-responses"
	APIAnthropicMessages     API = "anthropic-messages"
)

// Valid reports whether the API protocol is supported.
func (a API) Valid() bool {
	switch a {
	case APIOpenAIChatCompletions, APIOpenAIResponses, APIAnthropicMessages:
		return true
	default:
		return false
	}
}

// Definition is one model exposed by a provider.
type Definition struct {
	ID      string `json:"id"`
	Context int    `json:"context"`
	Alias   string `json:"alias"`
	API     API    `json:"api"`
}

// Provider owns connection credentials and the models available through it.
type Provider struct {
	ID      string       `json:"id"`
	Alias   string       `json:"alias,omitempty"`
	BaseURL string       `json:"base_url"`
	APIKey  string       `json:"api_key"`
	Models  []Definition `json:"models"`
}

// DisplayName returns the configured friendly name, falling back to the ID.
func (p Provider) DisplayName() string {
	if strings.TrimSpace(p.Alias) != "" {
		return p.Alias
	}
	return p.ID
}

// Resolved is a model definition combined with its provider connection.
// It is passed internally only and is never serialized to the browser.
type Resolved struct {
	Ref           string
	ProviderID    string
	ProviderAlias string
	BaseURL       string
	APIKey        string
	ID            string
	Context       int
	Alias         string
	API           API
}

// Reference returns the stable selector used in engines and arbiter fields.
func Reference(providerID, modelID string) string {
	return strings.TrimSpace(providerID) + "/" + strings.TrimSpace(modelID)
}

// ProviderName returns the provider's friendly name, falling back to its ID.
func (m Resolved) ProviderName() string {
	if strings.TrimSpace(m.ProviderAlias) != "" {
		return m.ProviderAlias
	}
	return m.ProviderID
}

// DisplayName returns the configured friendly name, falling back to the ID.
func (m Resolved) DisplayName() string {
	if strings.TrimSpace(m.Alias) != "" {
		return m.Alias
	}
	return m.ID
}
