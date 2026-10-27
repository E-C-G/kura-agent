package platform

// Linux : installation (compte kura-agent, /etc/kura), planification par systemd,
// coffre par systemd-creds, CLI AWS en zip signé PGP ; icône et fermeture
// d'applications à venir.
var implemented = map[Feature]bool{Install: true, Scheduling: true, Vault: true, AWSCLISetup: true}
