package main

import (
	"encoding/json"
	"fmt"
)

// MessageKind classifies a JSON-RPC 2.0 message.
type MessageKind int

const (
	KindRequest      MessageKind = iota // has method + id
	KindNotification                    // has method, no id
	KindResponse                        // has id, no method
	KindInvalid                         // neither
)

// Envelope is the JSON-RPC 2.0 envelope. We parse only the routing
// fields (id, method) and leave params/result/error as raw bytes.
type Envelope struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   json.RawMessage  `json:"error,omitempty"`
}

func parseEnvelope(line []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	return &env, nil
}

func classify(env *Envelope) MessageKind {
	hasMethod := env.Method != ""
	hasID := env.ID != nil
	switch {
	case hasMethod && hasID:
		return KindRequest
	case hasMethod && !hasID:
		return KindNotification
	case !hasMethod && hasID:
		return KindResponse
	default:
		return KindInvalid
	}
}

// rewriteID replaces the "id" field in a raw JSON-RPC message.
// Returns the modified JSON line.
func rewriteID(line []byte, newID int64) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	idBytes, err := json.Marshal(newID)
	if err != nil {
		return nil, err
	}
	raw["id"] = json.RawMessage(idBytes)
	return json.Marshal(raw)
}
