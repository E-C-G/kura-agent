package platform

// Linux : planification par systemd (internal/systemd), coffre par systemd-creds
// (internal/creds), CLI AWS en zip signé PGP (internal/awsinstall) ; le reste à venir.
var implemented = map[Feature]bool{Scheduling: true, Vault: true, AWSCLISetup: true}
