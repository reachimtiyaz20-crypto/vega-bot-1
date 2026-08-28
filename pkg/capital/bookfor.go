package capital

import (
	"errors"
	"os"
)

// BookFor loads one named book from configPath, persisting its holds in dataDir.
//
// A MISSING CONFIG FILE RETURNS (nil, nil), and a nil *Ledger means no ceiling.
// That is exactly the behaviour that existed before this package, so a machine
// that has not been configured yet keeps running rather than failing to start.
//
// A config file that exists but is malformed, or that does not contain the
// named book, is an error. That is a mistake rather than an absence, and
// starting anyway would mean running unbounded while believing otherwise --
// which is the failure this package was written to end.
func BookFor(configPath, dataDir, name string) (*Ledger, error) {
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	m, err := LoadManager(configPath, dataDir)
	if err != nil {
		return nil, err
	}
	return m.Book(name)
}
