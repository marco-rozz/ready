//go:build darwin || windows

package secretstore

// vaultService identifies READY's entries in the native credential stores.
const vaultService = "ready"
