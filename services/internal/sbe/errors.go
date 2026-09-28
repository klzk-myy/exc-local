// Shared sentinel errors for the sbe package (L3 protocol faults).
package sbe

import "errors"

var (
	errNilTransport = errors.New("sbe: nil transport")
	errNilJournal   = errors.New("sbe: nil journal")
)
