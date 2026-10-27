package creds

// System : coffre de production, chiffré par DPAPI dans dir.
func System(dir string) Store { return Store{Dir: dir} }
