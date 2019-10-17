package bootstrap

import (
	"bytes"
	"io"
)

type Redactor struct {
	replacement []byte

	// Current offset from the start of the next input segment
	offset int

	// Minimum and maximum length of redactable string
	minlen int
	maxlen int

	// Table of Boyer-Moore skip distances, and values to redact matching this end byte
	table [255]struct {
		skip    int
		needles [][]byte
	}

	// Internal buffer for building redacted input into
	// Also holds the final portion of the previous Write call, in case of
	// sensitive values that cross Write boundaries
	outbuf []byte

	// Wrapped Writer that we'll send redacted output to
	output io.Writer
}

// Construct a new Redactor, and pre-compile the Boyer-Moore skip table
func NewRedactor(output io.Writer, replacement string, needles []string) *Redactor {
	minNeedleLen := 0
	maxNeedleLen := 0
	for _, needle := range needles {
		if len(needle) < minNeedleLen || minNeedleLen == 0 {
			minNeedleLen = len(needle)
		}
		if len(needle) > maxNeedleLen {
			maxNeedleLen = len(needle)
		}
	}

	redactor := &Redactor{
		replacement: []byte(replacement),
		output:      output,

		// Linux pipes can buffer up to 65536 bytes before flushing, so there's
		// a reasonable chance that's how much we'll get in a single Write().
		// maxNeedleLen is added since we may retain that many bytes to handle
		// matches crossing Write boundaries.
		// It's a reasonable starting capacity which hopefully means we don't
		// have to reallocate the array, but append() will grow it if necessary
		outbuf: make([]byte, 0, 65536+maxNeedleLen),

		// Since Boyer-Moore looks for the end of substrings, we can safely offset
		// processing by the length of the shortest string we're checking for
		// Since Boyer-Moore looks for the end of substrings, only bytes further
		// behind the iterator than the longest search string are guaranteed to not
		// be part of a match
		minlen: minNeedleLen,
		maxlen: maxNeedleLen,
		offset: minNeedleLen - 1,
	}

	// For bytes that don't appear in any of the substrings we're searching
	// for, it's safe to skip forward the length of the shortest search
	// string.
	// Start by setting this as a default for all bytes
	for i := range redactor.table {
		redactor.table[i].skip = minNeedleLen
	}

	for _, needle := range needles {
		for i, ch := range needle {
			// For bytes that do exist in search strings, find the shortest distance
			// between that byte appearing to the end of the same search string
			skip := len(needle) - i - 1
			if skip < redactor.table[ch].skip {
				redactor.table[ch].skip = skip
			}

			// Build a cache of which search substrings end in which bytes
			if skip == 0 {
				redactor.table[ch].needles = append(redactor.table[ch].needles, []byte(needle))
			}
		}
	}

	return redactor
}

func (redactor *Redactor) Write(input []byte) (int, error) {
	// Current iterator index, which may be a safe offset from 0
	cursor := redactor.offset

	// Current index which is guaranteed to be completely redacted
	// May lag behind cursor by up to the length of the longest search string
	doneTo := 0

	for cursor < len(input) {
		ch := input[cursor]
		skip := redactor.table[ch].skip

		// If the skip table tells us that there is no search string ending in
		// the current byte, skip forward by the indicated distance.
		if skip != 0 {
			cursor += skip

			// Also copy any content behind the cursor which is guaranteed not
			// to fall under a match
			confirmedTo := cursor - redactor.maxlen - 1
			if confirmedTo > len(input) {
				confirmedTo = len(input)
			}
			if confirmedTo > doneTo {
				redactor.outbuf = append(redactor.outbuf, input[doneTo:confirmedTo]...)
				doneTo = confirmedTo
			}

			continue
		}

		// We'll check for matching search strings here, but we'll still need
		// to move the cursor forward
		// Since Go slice syntax is not inclusive of the end index, moving it
		// forward now reduces the need to use `cursor-1` everywhere
		cursor++
		for _, needle := range redactor.table[ch].needles {
			// Since we're working backwards from what may be the end of a
			// string, it's possible that the start would be out of bounds
			startSubstr := cursor - len(needle)
			var candidate []byte

			if startSubstr >= 0 {
				// If the candidate string falls entirely within input, then just slice into input
				candidate = input[startSubstr:cursor]
			} else if -startSubstr <= len(redactor.outbuf) {
				// If the candidate crosses the Write boundary, we need to
				// concatenate the two sections to compare against
				candidate = make([]byte, 0, len(needle))
				candidate = append(candidate, redactor.outbuf[-startSubstr-1:]...)
				candidate = append(candidate, input[:cursor]...)
			} else {
				// Final case is that the start index is out of bounds, and
				// it's impossible for it to match. Just move on to the next
				// search substring
				continue
			}

			if bytes.Equal(needle, candidate) {
				if startSubstr < 0 {
					// If we accepted a negative startSubstr, the output buffer
					// needs to be truncated to remove the partial match
					redactor.outbuf = redactor.outbuf[:len(redactor.outbuf)+startSubstr]
				} else if startSubstr > doneTo {
					// First, copy over anything behind the matched substring unmodified
					redactor.outbuf = append(redactor.outbuf, input[doneTo:startSubstr]...)
				}
				// Then, write a fixed string into the output, and move doneTo past the redaction
				redactor.outbuf = append(redactor.outbuf, redactor.replacement...)
				doneTo = cursor

				// The next end-of-string will be at least this far away so
				// it's safe to skip forward a bit
				cursor += redactor.minlen - 1
				break
			}
		}
	}

	// We buffer the end of the input in order to catch passwords that fall over Write boundaries.
	// In the case of line-buffered input, that means we would hold back the
	// end of the line in a user-visible way. For this reason, we push through
	// any line endings immediately rather than hold them back.
	// The \r case should help to handle progress bars/spinners that use \r to
	// overwrite the current line.
	// Technically this means that passwords containing newlines aren't
	// guarateed to get redacted, but who does that anyway?
	for i := doneTo; i < len(input); i++ {
		if input[i] == byte('\r') || input[i] == byte('\n') {
			redactor.outbuf = append(redactor.outbuf, input[doneTo:i+1]...)
			doneTo = i + 1
		}
	}

	var err error
	if doneTo > 0 {
		// Push the output buffer down
		_, err = redactor.output.Write(redactor.outbuf)

		// There will probably be a segment at the end of the input which may be a
		// partial match crossing the Write boundary. This is retained in the
		// output buffer to compare against on the next call
		// Flush() needs to be called after the final Write(), or this bit won't
		// get written
		redactor.outbuf = append(redactor.outbuf[:0], input[doneTo:]...)
	} else {
		// If nothing was done, just add what we got to the buffer to be
		// processed on the next run
		redactor.outbuf = append(redactor.outbuf, input...)
	}

	// We can offset the next Write processing by how far cursor is ahead of
	// the end of this input segment
	redactor.offset = cursor - len(input)

	return len(input), err
}

// Flush should be called after the final Write. This will Write() anything
// retained in case of a partial match and reset the output buffer.
func (redactor Redactor) Sync() error {
	_, err := redactor.output.Write(redactor.outbuf)
	redactor.outbuf = redactor.outbuf[:0]
	return err
}
