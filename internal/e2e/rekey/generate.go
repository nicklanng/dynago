// Package rekey is a test-only store whose second table generation changes an entity's key: people
// were keyed by id, and are keyed by email. See rekey.dynago.yaml.
package rekey

//go:generate go run github.com/nicklanng/dynago/cmd/dynago generate rekey.dynago.yaml
