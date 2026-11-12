// Package transcript holds the black-box contract tests for the transcript:
// docs/reference/providers.md §4.2 (schema and rules), §4.3 (time base, marks,
// pauses) and §6 (the transcript tests).
//
// The tests drive `recap tool normalize` and `recap tool assemble` from
// RECAP_BIN and skip when it is unset. Their I/O is defined in README.md in
// this directory. Inputs and goldens are under testdata/transcripts/, written
// by tools/ref/make_transcripts.py from fixture F-01.
package transcript
