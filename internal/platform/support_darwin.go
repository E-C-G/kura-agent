package platform

// macOS : installation (root, /Library/Application Support/Kura), planification par
// launchd, coffre chiffré par une clé du trousseau Système ; CLI AWS, icône et
// fermeture d'applications à venir. install reste refusé tant que l'installation de
// la CLI AWS n'est pas écrite.
var implemented = map[Feature]bool{Install: true, Scheduling: true, Vault: true}
