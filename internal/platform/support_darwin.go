package platform

// macOS : installation (root, /Library/Application Support/Kura), planification par
// launchd, coffre chiffré par une clé du trousseau Système, CLI AWS en paquet signé par
// Amazon ; icône et fermeture d'applications à venir.
var implemented = map[Feature]bool{Install: true, Scheduling: true, Vault: true, AWSCLISetup: true}
