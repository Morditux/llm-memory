package model2vec

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const defaultMaxLength = 512

type addedTokenJSON struct {
	ID      int    `json:"id"`
	Content string `json:"content"`
	Special bool   `json:"special"`
}

type tokenizerJSON struct {
	AddedTokens  []addedTokenJSON `json:"added_tokens"`
	Normalizer   json.RawMessage  `json:"normalizer"`
	PreTokenizer json.RawMessage  `json:"pre_tokenizer"`
	Model        json.RawMessage  `json:"model"`
}

// tokenizer turns raw text into token IDs (without special tokens, with UNK filtered).
type tokenizer struct {
	normalizer   normalizer
	preTokenizer preTokenizer
	model        tokenModel
	unkID        int
	hasUnk       bool
}

type normalizer interface {
	Normalize(string) string
}

type preTokenizer interface {
	PreTokenize(string) []string
}

type tokenModel interface {
	Tokenize(piece string) []int
	VocabSize() int
}

func loadTokenizer(path string) (*tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tokenizer: %w", err)
	}
	var tj tokenizerJSON
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, fmt.Errorf("parse tokenizer: %w", err)
	}

	normStep, err := parseNormalizer(tj.Normalizer)
	if err != nil {
		return nil, fmt.Errorf("tokenizer normalizer: %w", err)
	}
	preStep, err := parsePreTokenizer(tj.PreTokenizer)
	if err != nil {
		return nil, fmt.Errorf("tokenizer pre_tokenizer: %w", err)
	}
	tm, unkID, hasUnk, err := parseTokenModel(tj.Model, tj.AddedTokens)
	if err != nil {
		return nil, fmt.Errorf("tokenizer model: %w", err)
	}

	return &tokenizer{
		normalizer:   normStep,
		preTokenizer: preStep,
		model:        tm,
		unkID:        unkID,
		hasUnk:       hasUnk,
	}, nil
}

func (t *tokenizer) VocabSize() int { return t.model.VocabSize() }

func (t *tokenizer) Encode(text string, maxLength int) []int {
	if t.normalizer != nil {
		text = t.normalizer.Normalize(text)
	}
	var pieces []string
	if t.preTokenizer != nil {
		pieces = t.preTokenizer.PreTokenize(text)
	} else if text != "" {
		pieces = []string{text}
	}

	var ids []int
	for _, p := range pieces {
		if p == "" {
			continue
		}
		for _, id := range t.model.Tokenize(p) {
			if t.hasUnk && id == t.unkID {
				continue
			}
			ids = append(ids, id)
			if maxLength > 0 && len(ids) >= maxLength {
				return ids
			}
		}
	}
	return ids
}

// ---- Normalizers -----------------------------------------------------------

type typedNode struct {
	Type string `json:"type"`
}

func parseNormalizer(raw json.RawMessage) (normalizer, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var head typedNode
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	switch head.Type {
	case "BertNormalizer":
		var cfg struct {
			CleanText          bool  `json:"clean_text"`
			HandleChineseChars bool  `json:"handle_chinese_chars"`
			StripAccents       *bool `json:"strip_accents"`
			Lowercase          bool  `json:"lowercase"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		stripAccents := cfg.Lowercase
		if cfg.StripAccents != nil {
			stripAccents = *cfg.StripAccents
		}
		return bertNormalizer{
			cleanText:          cfg.CleanText,
			handleChineseChars: cfg.HandleChineseChars,
			stripAccents:       stripAccents,
			lowercase:          cfg.Lowercase,
		}, nil
	case "Sequence":
		var cfg struct {
			Normalizers []json.RawMessage `json:"normalizers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		steps := make([]normalizer, 0, len(cfg.Normalizers))
		for _, sub := range cfg.Normalizers {
			n, err := parseNormalizer(sub)
			if err != nil {
				return nil, err
			}
			if n != nil {
				steps = append(steps, n)
			}
		}
		return seqNormalizer(steps), nil
	case "Lowercase":
		return lowercaseNormalizer{}, nil
	case "NFC":
		return unicodeFormNormalizer{form: norm.NFC}, nil
	case "NFKC":
		return unicodeFormNormalizer{form: norm.NFKC}, nil
	case "NFD":
		return unicodeFormNormalizer{form: norm.NFD}, nil
	case "NFKD":
		return unicodeFormNormalizer{form: norm.NFKD}, nil
	case "Strip":
		var cfg struct {
			StripLeft  bool `json:"strip_left"`
			StripRight bool `json:"strip_right"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return stripNormalizer{left: cfg.StripLeft, right: cfg.StripRight}, nil
	case "Replace":
		var cfg struct {
			Pattern struct {
				String *string `json:"String"`
				Regex  *string `json:"Regex"`
			} `json:"pattern"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Pattern.String != nil {
			return replaceStringNormalizer{old: *cfg.Pattern.String, new: cfg.Content}, nil
		}
		if cfg.Pattern.Regex != nil {
			re, err := regexp.Compile(*cfg.Pattern.Regex)
			if err != nil {
				return nil, fmt.Errorf("compile replace regex %q: %w", *cfg.Pattern.Regex, err)
			}
			return replaceRegexNormalizer{re: re, repl: cfg.Content}, nil
		}
		return nil, errors.New("replace normalizer missing pattern")
	case "Precompiled":
		var cfg struct {
			Charsmap string `json:"precompiled_charsmap"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return parsePrecompiledNormalizer(cfg.Charsmap)
	default:
		return nil, fmt.Errorf("unsupported normalizer type %q", head.Type)
	}
}

type seqNormalizer []normalizer

func (s seqNormalizer) Normalize(text string) string {
	for _, n := range s {
		text = n.Normalize(text)
	}
	return text
}

type lowercaseNormalizer struct{}

func (lowercaseNormalizer) Normalize(s string) string { return strings.ToLower(s) }

type unicodeFormNormalizer struct {
	form norm.Form
}

func (u unicodeFormNormalizer) Normalize(s string) string { return u.form.String(s) }

type stripNormalizer struct {
	left, right bool
}

func (st stripNormalizer) Normalize(s string) string {
	if st.left && st.right {
		return strings.TrimSpace(s)
	}
	if st.left {
		return strings.TrimLeftFunc(s, unicode.IsSpace)
	}
	if st.right {
		return strings.TrimRightFunc(s, unicode.IsSpace)
	}
	return s
}

type replaceStringNormalizer struct {
	old, new string
}

func (r replaceStringNormalizer) Normalize(s string) string {
	return strings.ReplaceAll(s, r.old, r.new)
}

type replaceRegexNormalizer struct {
	re   *regexp.Regexp
	repl string
}

func (r replaceRegexNormalizer) Normalize(s string) string {
	return r.re.ReplaceAllString(s, r.repl)
}

type bertNormalizer struct {
	cleanText          bool
	handleChineseChars bool
	stripAccents       bool
	lowercase          bool
}

func (b bertNormalizer) Normalize(s string) string {
	if b.cleanText {
		var sb strings.Builder
		sb.Grow(len(s))
		for _, r := range s {
			if r == 0 || r == 0xFFFD || isBertControl(r) {
				continue
			}
			if isBertWhitespace(r) {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(r)
			}
		}
		s = sb.String()
	}
	if b.handleChineseChars {
		var sb strings.Builder
		sb.Grow(len(s))
		for _, r := range s {
			if isChineseChar(r) {
				sb.WriteByte(' ')
				sb.WriteRune(r)
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(r)
			}
		}
		s = sb.String()
	}
	if b.lowercase {
		s = strings.ToLower(s)
	}
	if b.stripAccents {
		s = stripCombiningMarks(s)
	}
	return s
}

func stripCombiningMarks(s string) string {
	decomposed := norm.NFD.String(s)
	var sb strings.Builder
	sb.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func isBertWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || unicode.Is(unicode.Zs, r)
}

func isBertControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cn, r)
}

func isChineseChar(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}

// precompiledNormalizer executes a SentencePiece DARTS double-array charsmap.
type precompiledNormalizer struct {
	trie []uint32
	pool []byte
}

func parsePrecompiledNormalizer(b64 string) (*precompiledNormalizer, error) {
	if b64 == "" {
		return &precompiledNormalizer{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode precompiled_charsmap: %w", err)
	}
	if len(raw) < 4 {
		return nil, errors.New("precompiled_charsmap too short")
	}
	trieSize := int(binary.LittleEndian.Uint32(raw[:4]))
	if trieSize%4 != 0 || 4+trieSize > len(raw) {
		return nil, fmt.Errorf("invalid precompiled_charsmap trie size %d", trieSize)
	}
	trie := make([]uint32, trieSize/4)
	for i := range trie {
		trie[i] = binary.LittleEndian.Uint32(raw[4+4*i:])
	}
	pool := make([]byte, len(raw)-4-trieSize)
	copy(pool, raw[4+trieSize:])
	return &precompiledNormalizer{trie: trie, pool: pool}, nil
}

func (p *precompiledNormalizer) Normalize(s string) string {
	if len(p.trie) == 0 || s == "" {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for pos := 0; pos < len(s); {
		matchLen, valIdx, ok := p.longestPrefix(s[pos:])
		if ok {
			repl := p.poolString(valIdx)
			sb.WriteString(repl)
			pos += matchLen
			continue
		}
		_, w := utf8.DecodeRuneInString(s[pos:])
		if w <= 0 {
			w = 1
		}
		sb.WriteString(s[pos : pos+w])
		pos += w
	}
	return sb.String()
}

func (p *precompiledNormalizer) longestPrefix(s string) (matchLen int, valIdx int, ok bool) {
	nodePos := uint32(0)
	unit := p.trie[nodePos]
	nodePos ^= dartsOffset(unit)
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0 {
			break
		}
		nodePos ^= uint32(b)
		if int(nodePos) >= len(p.trie) {
			break
		}
		unit = p.trie[nodePos]
		if dartsLabel(unit) != uint32(b) {
			break
		}
		nodePos ^= dartsOffset(unit)
		if dartsHasLeaf(unit) {
			if int(nodePos) >= len(p.trie) {
				break
			}
			v := p.trie[nodePos]
			matchLen = i + 1
			valIdx = int(dartsValue(v))
			ok = true
		}
	}
	return matchLen, valIdx, ok
}

func (p *precompiledNormalizer) poolString(idx int) string {
	if idx < 0 || idx >= len(p.pool) {
		return ""
	}
	end := idx
	for end < len(p.pool) && p.pool[end] != 0 {
		end++
	}
	return string(p.pool[idx:end])
}

func dartsHasLeaf(u uint32) bool { return ((u >> 8) & 1) != 0 }
func dartsValue(u uint32) uint32 { return u & ((1 << 31) - 1) }
func dartsLabel(u uint32) uint32 { return u & ((1 << 31) | 0xFF) }
func dartsOffset(u uint32) uint32 {
	return (u >> 10) << ((u & (1 << 9)) >> 6)
}

// ---- PreTokenizers ---------------------------------------------------------

func parsePreTokenizer(raw json.RawMessage) (preTokenizer, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var head typedNode
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	switch head.Type {
	case "BertPreTokenizer":
		return bertPreTokenizer{}, nil
	case "Whitespace":
		return whitespacePreTokenizer{splitPunct: true}, nil
	case "WhitespaceSplit":
		return whitespacePreTokenizer{splitPunct: false}, nil
	case "Metaspace":
		var cfg struct {
			Replacement   string `json:"replacement"`
			PrependScheme string `json:"prepend_scheme"`
			AddPrefix     *bool  `json:"add_prefix_space"`
			Split         *bool  `json:"split"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		repl := cfg.Replacement
		if repl == "" {
			repl = "▁"
		}
		scheme := cfg.PrependScheme
		if scheme == "" {
			if cfg.AddPrefix != nil && !*cfg.AddPrefix {
				scheme = "never"
			} else {
				scheme = "always"
			}
		}
		split := true
		if cfg.Split != nil {
			split = *cfg.Split
		}
		return metaspacePreTokenizer{
			replacement:   repl,
			prependScheme: scheme,
			split:         split,
		}, nil
	case "Sequence":
		var cfg struct {
			PreTokenizers []json.RawMessage `json:"pretokenizers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		steps := make([]preTokenizer, 0, len(cfg.PreTokenizers))
		for _, sub := range cfg.PreTokenizers {
			p, err := parsePreTokenizer(sub)
			if err != nil {
				return nil, err
			}
			if p != nil {
				steps = append(steps, p)
			}
		}
		return seqPreTokenizer(steps), nil
	default:
		return nil, fmt.Errorf("unsupported pre_tokenizer type %q", head.Type)
	}
}

type seqPreTokenizer []preTokenizer

func (s seqPreTokenizer) PreTokenize(text string) []string {
	cur := []string{text}
	for _, p := range s {
		next := make([]string, 0, len(cur))
		for _, item := range cur {
			next = append(next, p.PreTokenize(item)...)
		}
		cur = next
	}
	return cur
}

type bertPreTokenizer struct{}

func (bertPreTokenizer) PreTokenize(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isBertPunctuation(r):
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func isBertPunctuation(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

type whitespacePreTokenizer struct {
	splitPunct bool
}

func (w whitespacePreTokenizer) PreTokenize(text string) []string {
	if !w.splitPunct {
		return strings.Fields(text)
	}
	return bertPreTokenizer{}.PreTokenize(text)
}

type metaspacePreTokenizer struct {
	replacement   string
	prependScheme string
	split         bool
}

func (m metaspacePreTokenizer) PreTokenize(text string) []string {
	if text == "" {
		return nil
	}
	replaced := strings.ReplaceAll(text, " ", m.replacement)
	if m.prependScheme != "never" && !strings.HasPrefix(replaced, m.replacement) {
		replaced = m.replacement + replaced
	}
	if !m.split {
		return []string{replaced}
	}
	var out []string
	start := 0
	for i := 0; i < len(replaced); {
		if i > start && strings.HasPrefix(replaced[i:], m.replacement) {
			out = append(out, replaced[start:i])
			start = i
			i += len(m.replacement)
			continue
		}
		_, w := utf8.DecodeRuneInString(replaced[i:])
		if w <= 0 {
			w = 1
		}
		i += w
	}
	if start < len(replaced) {
		out = append(out, replaced[start:])
	}
	return out
}

// ---- Token Models ----------------------------------------------------------

func parseTokenModel(raw json.RawMessage, added []addedTokenJSON) (tokenModel, int, bool, error) {
	var head typedNode
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, 0, false, err
	}
	switch head.Type {
	case "WordPiece":
		var cfg struct {
			UnkToken                string         `json:"unk_token"`
			ContinuingSubwordPrefix string         `json:"continuing_subword_prefix"`
			MaxInputCharsPerWord    int            `json:"max_input_chars_per_word"`
			Vocab                   map[string]int `json:"vocab"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, 0, false, err
		}
		if cfg.Vocab == nil {
			cfg.Vocab = map[string]int{}
		}
		for _, a := range added {
			if _, exists := cfg.Vocab[a.Content]; !exists {
				cfg.Vocab[a.Content] = a.ID
			}
		}
		if cfg.ContinuingSubwordPrefix == "" {
			cfg.ContinuingSubwordPrefix = "##"
		}
		if cfg.MaxInputCharsPerWord <= 0 {
			cfg.MaxInputCharsPerWord = 100
		}
		unkID, hasUnk := cfg.Vocab[cfg.UnkToken]
		return &wordPieceModel{
			vocab:        cfg.Vocab,
			unkID:        unkID,
			hasUnk:       hasUnk,
			prefix:       cfg.ContinuingSubwordPrefix,
			maxWordRunes: cfg.MaxInputCharsPerWord,
		}, unkID, hasUnk, nil

	case "Unigram":
		var cfg struct {
			UnkID        *int                `json:"unk_id"`
			Vocab        [][]json.RawMessage `json:"vocab"`
			ByteFallback bool                `json:"byte_fallback"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, 0, false, err
		}
		m, unkID, hasUnk, err := buildUnigramModel(cfg.Vocab, cfg.UnkID, cfg.ByteFallback, added)
		if err != nil {
			return nil, 0, false, err
		}
		return m, unkID, hasUnk, nil

	default:
		return nil, 0, false, fmt.Errorf("unsupported tokenizer model type %q", head.Type)
	}
}

type wordPieceModel struct {
	vocab        map[string]int
	unkID        int
	hasUnk       bool
	prefix       string
	maxWordRunes int
}

func (w *wordPieceModel) VocabSize() int { return len(w.vocab) }

func (w *wordPieceModel) Tokenize(word string) []int {
	if word == "" {
		return nil
	}
	if utf8.RuneCountInString(word) > w.maxWordRunes {
		if w.hasUnk {
			return []int{w.unkID}
		}
		return nil
	}

	// Track byte offsets of rune boundaries so greedy slicing never splits a rune.
	offsets := make([]int, 0, len(word)+1)
	for i := range word {
		offsets = append(offsets, i)
	}
	offsets = append(offsets, len(word))

	n := len(offsets) - 1
	subTokens := make([]int, 0, 4)
	start := 0
	for start < n {
		end := n
		matched := false
		for end > start {
			sub := word[offsets[start]:offsets[end]]
			if start > 0 {
				sub = w.prefix + sub
			}
			if id, ok := w.vocab[sub]; ok {
				subTokens = append(subTokens, id)
				matched = true
				break
			}
			end--
		}
		if !matched {
			if w.hasUnk {
				return []int{w.unkID}
			}
			return nil
		}
		start = end
	}
	return subTokens
}

// unigramModel implements SentencePiece Unigram Viterbi tokenization using a compact byte trie.
type unigramModel struct {
	nodes        []unigramTrieNode
	byteTokens   [256]int
	hasByteToken [256]bool
	byteFallback bool
	unkID        int
	hasUnk       bool
	unkScore     float64
	vocabSize    int
}

type unigramEdge struct {
	b    byte
	node int32
}

type unigramTrieNode struct {
	id       int32
	score    float64
	hasToken bool
	edges    []unigramEdge
}

func buildUnigramModel(rawVocab [][]json.RawMessage, unkIDPtr *int, byteFallback bool, added []addedTokenJSON) (*unigramModel, int, bool, error) {
	m := &unigramModel{
		nodes:        make([]unigramTrieNode, 1),
		byteFallback: byteFallback,
	}
	minScore := 0.0
	hasMin := false
	maxID := -1

	insert := func(piece string, id int, score float64) {
		if id > maxID {
			maxID = id
		}
		if !hasMin || score < minScore {
			minScore = score
			hasMin = true
		}
		if piece == "" {
			return
		}
		cur := 0
		for i := 0; i < len(piece); i++ {
			b := piece[i]
			next := -1
			for _, e := range m.nodes[cur].edges {
				if e.b == b {
					next = int(e.node)
					break
				}
			}
			if next < 0 {
				next = len(m.nodes)
				m.nodes = append(m.nodes, unigramTrieNode{})
				m.nodes[cur].edges = append(m.nodes[cur].edges, unigramEdge{b: b, node: int32(next)})
			}
			cur = next
		}
		m.nodes[cur].id = int32(id)
		m.nodes[cur].score = score
		m.nodes[cur].hasToken = true
	}

	for id, pair := range rawVocab {
		if len(pair) != 2 {
			return nil, 0, false, fmt.Errorf("invalid unigram vocab entry at index %d", id)
		}
		var piece string
		if err := json.Unmarshal(pair[0], &piece); err != nil {
			return nil, 0, false, fmt.Errorf("unigram vocab[%d] piece: %w", id, err)
		}
		var score float64
		if err := json.Unmarshal(pair[1], &score); err != nil {
			return nil, 0, false, fmt.Errorf("unigram vocab[%d] score: %w", id, err)
		}
		insert(piece, id, score)
		if byteFallback && len(piece) == 6 && strings.HasPrefix(piece, "<0x") && piece[5] == '>' {
			if bv, ok := parseHexByte(piece[3:5]); ok {
				m.byteTokens[bv] = id
				m.hasByteToken[bv] = true
			}
		}
	}

	for _, a := range added {
		if a.ID > maxID {
			maxID = a.ID
		}
	}
	m.vocabSize = max(len(rawVocab), maxID+1)
	if unkIDPtr != nil && *unkIDPtr >= 0 {
		m.unkID = *unkIDPtr
		m.hasUnk = true
	}
	m.unkScore = minScore - 10.0
	return m, m.unkID, m.hasUnk, nil
}

func parseHexByte(s string) (byte, bool) {
	if len(s) != 2 {
		return 0, false
	}
	var out byte
	for i := range 2 {
		c := s[i]
		out <<= 4
		switch {
		case c >= '0' && c <= '9':
			out |= c - '0'
		case c >= 'A' && c <= 'F':
			out |= c - 'A' + 10
		case c >= 'a' && c <= 'f':
			out |= c - 'a' + 10
		default:
			return 0, false
		}
	}
	return out, true
}

func (m *unigramModel) VocabSize() int { return m.vocabSize }

type viterbiStep struct {
	score float64
	prev  int
	id    int
	set   bool
}

func (m *unigramModel) Tokenize(word string) []int {
	n := len(word)
	if n == 0 {
		return nil
	}
	dp := make([]viterbiStep, n+1)
	dp[0].set = true

	for i := 0; i < n; {
		_, runeLen := utf8.DecodeRuneInString(word[i:])
		if runeLen <= 0 {
			runeLen = 1
		}
		if !dp[i].set {
			i += runeLen
			continue
		}

		// Walk the trie from word[i:].
		matchedSingleRune := false
		cur := 0
		for j := i; j < n; j++ {
			b := word[j]
			next := -1
			for _, e := range m.nodes[cur].edges {
				if e.b == b {
					next = int(e.node)
					break
				}
			}
			if next < 0 {
				break
			}
			cur = next
			node := &m.nodes[cur]
			if node.hasToken {
				end := j + 1
				if end == i+runeLen {
					matchedSingleRune = true
				}
				cand := dp[i].score + node.score
				if !dp[end].set || cand > dp[end].score {
					dp[end] = viterbiStep{score: cand, prev: i, id: int(node.id), set: true}
				}
			}
		}

		// Fallback for a single UTF-8 character that wasn't matched as a single token.
		if !matchedSingleRune {
			end := i + runeLen
			if m.byteFallback && m.allBytesKnown(word[i:end]) {
				// Emit each byte sequentially in the DP table.
				pos := i
				for k := i; k < end; k++ {
					cand := dp[pos].score + m.unkScore
					if !dp[pos+1].set || cand > dp[pos+1].score {
						dp[pos+1] = viterbiStep{score: cand, prev: pos, id: m.byteTokens[word[k]], set: true}
					}
					pos++
				}
			} else if m.hasUnk {
				cand := dp[i].score + m.unkScore
				if !dp[end].set || cand > dp[end].score {
					dp[end] = viterbiStep{score: cand, prev: i, id: m.unkID, set: true}
				}
			}
		}
		i += runeLen
	}

	if !dp[n].set {
		if m.hasUnk {
			return []int{m.unkID}
		}
		return nil
	}

	var rev []int
	for pos := n; pos > 0; pos = dp[pos].prev {
		rev = append(rev, dp[pos].id)
	}
	out := make([]int, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

func (m *unigramModel) allBytesKnown(s string) bool {
	for i := 0; i < len(s); i++ {
		if !m.hasByteToken[s[i]] {
			return false
		}
	}
	return true
}
