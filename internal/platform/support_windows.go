package platform

// Windows : toutes les fonctions sont écrites.
var implemented = map[Feature]bool{
	Scheduling: true, Vault: true, Install: true, AWSCLISetup: true, Tray: true, AppControl: true, SQLServerExport: true,
}
