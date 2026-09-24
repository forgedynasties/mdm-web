package api

import "regexp"

// serialRe is what a device serial can look like: letters, digits, '-' and '_' — a T7's
// AT070AABU00429, MDM-lite's android-<ANDROID_ID>, a corrupted T7's msm-<ANDROID_ID>.
//
// A serial with anything else in it is not a serial. T7s with a corrupted bootloader
// read "androidboot.baseband=msm" (the next token of the kernel command line) as their
// serial, every one of them the same, and the server merged all of them into a single
// device: 19 boots and 29 IP addresses taking turns on one record, 3.1M samples of
// several tablets' history interleaved. Such a check-in is refused outright — nothing is
// stored and no device is created. Client 1.4.7+ identifies as msm-<ANDROID_ID> instead.
var serialRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validSerial reports whether s can be a device serial (see serialRe).
func validSerial(s string) bool { return serialRe.MatchString(s) }

// errInvalidSerial is the body a refused check-in gets back.
const errInvalidSerial = "serial_number is not a device serial (corrupted serial; update the client to 1.4.7+)"
