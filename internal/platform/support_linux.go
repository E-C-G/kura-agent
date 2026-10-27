package platform

// Linux : planification par systemd (internal/systemd), coffre par systemd-creds
// (internal/creds) ; le reste à venir.
var implemented = map[Feature]bool{Scheduling: true, Vault: true}
