package backup

import (
	"bytes"
	"errors"
	"io"
	"strings"
)

const (
	extensionCommentPrefix = "COMMENT ON EXTENSION "
	// pg_dump 18 sets this GUC in its preamble and no pre-18 server knows it.
	transactionTimeoutPrefix = "SET transaction_timeout "
	beginStatement           = "BEGIN;"
	commitStatement          = "COMMIT;"
	copyPrefix               = "COPY "
	copyStdinTail            = "FROM stdin;"
	copyTerminator           = `\.`
	commentStart             = "--"
	abortTag                 = "$dbtether$"
	statementSpecials        = `'"$-;`

	scriptBufferSize  = 64 << 10
	maxDollarTagBytes = 63
	boundaryLookahead = len(transactionTimeoutPrefix)
	copyLookahead     = len(copyTerminator) + 1
	maxEmptyReads     = 100
)

type scriptState int

const (
	atBoundary scriptState = iota
	inStatement
	inLiteral
	inIdentifier
	inDollar
	inComment
	inCopy
	inMeta
	inDrop
)

// pg_dump wraps large objects in BEGIN/COMMIT, which under --single-transaction would commit everything before them.
func newRestoreScript(r io.Reader) *restoreScript {
	return &restoreScript{
		src:       r,
		buf:       make([]byte, scriptBufferSize),
		lineStart: true,
	}
}

type restoreScript struct {
	src io.Reader
	buf []byte
	r   int
	w   int

	state        scriptState
	afterRun     scriptState
	afterComment scriptState
	run          int

	dollarTag     []byte
	tail          []byte
	copyStatement bool
	dropping      bool
	lineStart     bool

	pending    []byte
	statements int
	headBytes  int
	content    bool
	srcErr     error
	finished   bool
	emitted    bool
	last       byte
	emptyReads int
}

func (s *restoreScript) Statements() int {
	return s.statements
}

// Lets a caller judge the head without depending on its own read sizes.
func (s *restoreScript) HeadBytes() int {
	return s.headBytes
}

func (s *restoreScript) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(s.pending) > 0 {
			n += s.emitPending(p[n:])
			continue
		}
		if s.finished {
			break
		}

		n += s.step(p[n:])
		if n == len(p) {
			break
		}
		if s.srcErr != nil {
			s.finish()
			continue
		}
		if n > 0 {
			break
		}
		s.fill()
	}

	if n == 0 && s.finished && len(s.pending) == 0 {
		return 0, s.srcErr
	}
	return n, nil
}

func (s *restoreScript) emitPending(out []byte) int {
	n := copy(out, s.pending)
	s.pending = s.pending[n:]
	return n
}

func (s *restoreScript) step(out []byte) int {
	written := 0
	for written < len(out) && s.r < s.w {
		n, decided := s.dispatch(s.buf[s.r:s.w], out[written:])
		written += n
		if !decided {
			break
		}
	}
	return written
}

func (s *restoreScript) dispatch(data, out []byte) (int, bool) {
	switch s.state {
	case inStatement:
		return s.stepStatement(data, out)
	case inLiteral:
		return s.stepLiteral(data, out)
	case inIdentifier:
		return s.stepIdentifier(data, out)
	case inDollar:
		return s.stepDollar(data, out)
	case inComment:
		return s.stepComment(data, out)
	case inCopy:
		return s.stepCopy(data, out)
	case inMeta:
		return s.stepMeta(data, out)
	case inDrop:
		return s.stepDrop(data)
	}
	return s.stepBoundary(data, out)
}

func (s *restoreScript) stepBoundary(data, out []byte) (int, bool) {
	if s.lineStart {
		if len(data) < boundaryLookahead && s.srcErr == nil {
			return 0, false
		}
		if s.matchColumnZero(data) {
			return 0, true
		}
	}

	if c := data[0]; c == '\n' || c == '\r' || c == ' ' || c == '\t' {
		_, written := s.pass(data, out, 1)
		return written, true
	}

	s.state = inStatement
	return 0, true
}

func (s *restoreScript) matchColumnZero(data []byte) bool {
	switch {
	case hasPrefix(data, extensionCommentPrefix), hasPrefix(data, beginStatement),
		hasPrefix(data, commitStatement), hasPrefix(data, transactionTimeoutPrefix):
		s.dropping = true
		s.state = inStatement
	case data[0] == '\\':
		s.state = inMeta
	case hasPrefix(data, commentStart):
		s.afterComment = atBoundary
		s.state = inComment
	case hasPrefix(data, copyPrefix):
		s.copyStatement = true
		s.tail = s.tail[:0]
		s.state = inStatement
	default:
		return false
	}
	return true
}

func (s *restoreScript) stepStatement(data, out []byte) (int, bool) {
	if written, running := s.drainRun(data, out); running {
		return written, true
	}

	i := bytes.IndexAny(data, statementSpecials)
	if i != 0 {
		if i < 0 {
			i = len(data)
		}
		_, written := s.pass(data, out, i)
		return written, true
	}

	switch data[0] {
	case '\'':
		_, written := s.pass(data, out, 1)
		s.state = inLiteral
		return written, true
	case '"':
		_, written := s.pass(data, out, 1)
		s.state = inIdentifier
		return written, true
	case ';':
		return s.endStatement(data, out)
	case '$':
		return s.openDollar(data, out)
	}
	return s.stepDash(data, out)
}

func (s *restoreScript) endStatement(data, out []byte) (int, bool) {
	_, written := s.pass(data, out, 1)
	switch {
	case s.dropping:
		s.state = inDrop
	case s.copyStatement && hasSuffix(s.tail, copyStdinTail):
		s.state = inCopy
	default:
		s.state = atBoundary
	}
	if !s.dropping && s.content {
		s.statements++
	}
	s.content = false
	s.copyStatement = false
	return written, true
}

func (s *restoreScript) stepDash(data, out []byte) (int, bool) {
	if len(data) < len(commentStart) && s.srcErr == nil {
		return 0, false
	}

	_, written := s.pass(data, out, 1)
	if len(data) > 1 && data[1] == '-' {
		s.afterComment = inStatement
		s.state = inComment
	}
	return written, true
}

func (s *restoreScript) stepLiteral(data, out []byte) (int, bool) {
	return s.passQuoted(data, out, '\'')
}

func (s *restoreScript) stepIdentifier(data, out []byte) (int, bool) {
	return s.passQuoted(data, out, '"')
}

// A doubled quote needs no case of its own: it closes the quoted run and reopens it on the very next byte.
func (s *restoreScript) passQuoted(data, out []byte, quote byte) (int, bool) {
	if written, running := s.drainRun(data, out); running {
		return written, true
	}

	if i := bytes.IndexByte(data, quote); i >= 0 {
		s.startRun(i+1, inStatement)
		return 0, true
	}

	_, written := s.pass(data, out, len(data))
	return written, true
}

func (s *restoreScript) openDollar(data, out []byte) (int, bool) {
	length, undecided := scanDollarTag(data)
	if undecided && s.srcErr == nil {
		return 0, false
	}
	if length == 0 {
		_, written := s.pass(data, out, 1)
		return written, true
	}

	s.dollarTag = append(s.dollarTag[:0], data[:length]...)
	s.startRun(length, inDollar)
	return 0, true
}

func (s *restoreScript) stepDollar(data, out []byte) (int, bool) {
	if written, running := s.drainRun(data, out); running {
		return written, true
	}

	if i := bytes.Index(data, s.dollarTag); i >= 0 {
		s.startRun(i+len(s.dollarTag), inStatement)
		return 0, true
	}

	n := len(data)
	if s.srcErr == nil {
		// the closing tag may straddle the end of the buffer
		n -= len(s.dollarTag) - 1
		if n <= 0 {
			return 0, false
		}
	}
	_, written := s.pass(data, out, n)
	return written, true
}

func (s *restoreScript) stepComment(data, out []byte) (int, bool) {
	return s.passLine(data, out, s.afterComment)
}

func (s *restoreScript) stepMeta(data, out []byte) (int, bool) {
	return s.passLine(data, out, atBoundary)
}

func (s *restoreScript) passLine(data, out []byte, next scriptState) (int, bool) {
	if written, running := s.drainRun(data, out); running {
		return written, true
	}

	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		s.startRun(i+1, next)
		return 0, true
	}

	_, written := s.pass(data, out, len(data))
	return written, true
}

func (s *restoreScript) stepCopy(data, out []byte) (int, bool) {
	if written, running := s.drainRun(data, out); running {
		return written, true
	}

	if s.lineStart {
		if len(data) < copyLookahead && s.srcErr == nil {
			return 0, false
		}
		if isCopyTerminator(data) {
			s.startRun(len(copyTerminator), atBoundary)
			return 0, true
		}
	}

	n := len(data)
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		n = i + 1
	}
	_, written := s.pass(data, out, n)
	return written, true
}

func (s *restoreScript) stepDrop(data []byte) (int, bool) {
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		s.consume(data, len(data))
		return 0, true
	}

	s.consume(data, i+1)
	s.dropping = false
	s.state = atBoundary
	return 0, true
}

func (s *restoreScript) startRun(n int, next scriptState) {
	s.run = n
	s.afterRun = next
}

// The state must not advance mid-pattern: it switches once the run's last byte is out.
func (s *restoreScript) drainRun(data, out []byte) (int, bool) {
	if s.run == 0 {
		return 0, false
	}

	consumed, written := s.pass(data, out, min(s.run, len(data)))
	s.run -= consumed
	if s.run == 0 {
		s.state = s.afterRun
	}
	return written, true
}

func (s *restoreScript) pass(data, out []byte, n int) (consumed, written int) {
	if s.dropping {
		s.consume(data, n)
		return n, 0
	}

	if n > len(out) {
		n = len(out)
	}
	copy(out, data[:n])
	s.consume(data, n)
	if n > 0 {
		s.last = data[n-1]
		s.emitted = true
		if s.statements == 0 {
			s.headBytes += n
		}
		if !s.content {
			s.content = hasContent(data[:n])
		}
	}
	return n, n
}

// A run of bare terminators is not a statement psql would send anywhere.
func hasContent(data []byte) bool {
	for _, c := range data {
		switch c {
		case ' ', '\t', '\r', '\n', ';':
			continue
		}
		return true
	}
	return false
}

func (s *restoreScript) consume(data []byte, n int) {
	if n == 0 {
		return
	}

	s.lineStart = data[n-1] == '\n'
	if s.copyStatement {
		s.tail = appendTail(s.tail, data[:n])
	}
	s.r += n
}

func (s *restoreScript) fill() {
	if s.r > 0 {
		s.w = copy(s.buf, s.buf[s.r:s.w])
		s.r = 0
	}

	read, err := s.src.Read(s.buf[s.w:])
	s.w += read
	switch {
	case err != nil:
		s.srcErr = err
	case read == 0:
		s.emptyReads++
		if s.emptyReads >= maxEmptyReads {
			s.srcErr = io.ErrNoProgress
		}
	default:
		s.emptyReads = 0
	}
}

func (s *restoreScript) finish() {
	s.finished = true
	if errors.Is(s.srcErr, io.EOF) {
		return
	}
	s.pending = []byte(s.abortTrailer(s.srcErr))
}

func (s *restoreScript) abortTrailer(cause error) string {
	var trailer strings.Builder
	if s.emitted && s.last != '\n' {
		trailer.WriteByte('\n')
	}
	if s.state == inCopy {
		trailer.WriteString(copyTerminator + "\n")
	}
	trailer.WriteString("DO " + abortTag + " BEGIN RAISE EXCEPTION " +
		quoteLiteral(truncationMessage(cause)) + "; END " + abortTag + ";\n")
	return trailer.String()
}

// The message ends up inside a dollar-quoted body, so it must not carry a '$' of its own.
func truncationMessage(cause error) string {
	stripped := strings.ReplaceAll(cause.Error(), "$", "")
	return "dbtether: backup stream truncated: " + strings.Join(strings.Fields(stripped), " ")
}

func scanDollarTag(data []byte) (length int, undecided bool) {
	limit := min(len(data), maxDollarTagBytes+2)
	for i := 1; i < limit; i++ {
		if data[i] == '$' {
			return i + 1, false
		}
		if !isTagByte(data[i], i == 1) {
			return 0, false
		}
	}
	return 0, len(data) < maxDollarTagBytes+2
}

func isTagByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}

func isCopyTerminator(data []byte) bool {
	if !hasPrefix(data, copyTerminator) {
		return false
	}
	return len(data) == len(copyTerminator) || data[len(copyTerminator)] == '\n' || data[len(copyTerminator)] == '\r'
}

func appendTail(tail, data []byte) []byte {
	if len(data) >= len(copyStdinTail) {
		return append(tail[:0], data[len(data)-len(copyStdinTail):]...)
	}

	tail = append(tail, data...)
	if len(tail) > len(copyStdinTail) {
		tail = append(tail[:0], tail[len(tail)-len(copyStdinTail):]...)
	}
	return tail
}

func hasPrefix(data []byte, prefix string) bool {
	return len(data) >= len(prefix) && string(data[:len(prefix)]) == prefix
}

func hasSuffix(data []byte, suffix string) bool {
	return len(data) >= len(suffix) && string(data[len(data)-len(suffix):]) == suffix
}
