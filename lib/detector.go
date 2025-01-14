package lib

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

//go:generate moq --out mocks/sample_updater.go --pkg mocks --skip-ensure . SampleUpdater
//go:generate moq --out mocks/http_client.go --pkg mocks --skip-ensure . HTTPClient

// Detector is a spam detector, thread-safe.
// It uses a set of checks to determine if a message is spam, and also keeps a list of approved users.
type Detector struct {
	Config
	classifier     classifier
	openaiChecker  *openAIChecker
	tokenizedSpam  []map[string]int
	approvedUsers  map[string]int
	stopWords      []string
	excludedTokens []string

	spamSamplesUpd SampleUpdater
	hamSamplesUpd  SampleUpdater

	lock sync.RWMutex
}

// Config is a set of parameters for Detector.
type Config struct {
	SimilarityThreshold float64    // threshold for spam similarity, 0.0 - 1.0
	MinMsgLen           int        // minimum message length to check
	MaxAllowedEmoji     int        // maximum number of emojis allowed in a message
	CasAPI              string     // CAS API URL
	FirstMessageOnly    bool       // if true, only the first message from a user is checked
	FirstMessagesCount  int        // number of first messages to check for spam
	HTTPClient          HTTPClient // http client to use for requests
	MinSpamProbability  float64    // minimum spam probability to consider a message spam with classifier, if 0 - ignored
	OpenAIVeto          bool       // if true, openai will be used to veto spam messages, otherwise it will be used to veto ham messages
}

// CheckResult is a result of spam check.
type CheckResult struct {
	Name    string `json:"name"`    // name of the check
	Spam    bool   `json:"spam"`    // true if spam
	Details string `json:"details"` // details of the check
}

// LoadResult is a result of loading samples.
type LoadResult struct {
	ExcludedTokens int // number of excluded tokens
	SpamSamples    int // number of spam samples
	HamSamples     int // number of ham samples
	StopWords      int // number of stop words (phrases)
}

// SampleUpdater is an interface for updating spam/ham samples on the fly.
type SampleUpdater interface {
	Append(msg string) error        // append a message to the samples storage
	Reader() (io.ReadCloser, error) // return a reader for the samples storage
}

// HTTPClient is an interface for http client, satisfied by http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewDetector makes a new Detector with the given config.
func NewDetector(p Config) *Detector {
	res := &Detector{
		Config:        p,
		classifier:    newClassifier(),
		approvedUsers: make(map[string]int),
		tokenizedSpam: []map[string]int{},
	}
	// if FirstMessagesCount is set, FirstMessageOnly enforced to true.
	// this is to avoid confusion when FirstMessagesCount is set but FirstMessageOnly is false.
	// the reason for the redundant FirstMessageOnly flag is to avoid breaking api compatibility.
	if p.FirstMessagesCount > 0 {
		res.FirstMessageOnly = true
	}
	return res
}

// WithOpenAIChecker sets an openAIChecker for spam checking.
func (d *Detector) WithOpenAIChecker(client openAIClient, config OpenAIConfig) {
	d.openaiChecker = newOpenAIChecker(client, config)
}

// Check checks if a given message is spam. Returns true if spam and also returns a list of check results.
func (d *Detector) Check(msg, userID string) (spam bool, cr []CheckResult) {

	isSpamDetected := func(cr []CheckResult) bool {
		for _, r := range cr {
			if r.Spam {
				return true
			}
		}
		return false
	}

	d.lock.RLock()
	defer d.lock.RUnlock()

	// approved user don't need to be checked
	if d.FirstMessageOnly && d.approvedUsers[userID] > d.FirstMessagesCount {
		return false, []CheckResult{{Name: "pre-approved", Spam: false, Details: "user already approved"}}
	}

	// all the checks are performed sequentially, so we can collect all the results

	// check for stop words if any stop words are loaded
	if len(d.stopWords) > 0 {
		cr = append(cr, d.isStopWord(msg))
	}

	// check for emojis if max allowed emojis is set
	if d.MaxAllowedEmoji >= 0 {
		cr = append(cr, d.isManyEmojis(msg))
	}

	// check for message length exceed the minimum size, if min message length is set.
	// the check is done after first simple checks, because stop words and emojis can be triggered by short messages as well.
	if len([]rune(msg)) < d.MinMsgLen {
		cr = append(cr, CheckResult{Name: "message length", Spam: false, Details: "too short"})
		if isSpamDetected(cr) {
			return true, cr // spam from checks above
		}
		return false, cr
	}

	// check for spam similarity  if similarity threshold is set and spam samples are loaded
	if d.SimilarityThreshold > 0 && len(d.tokenizedSpam) > 0 {
		cr = append(cr, d.isSpamSimilarityHigh(msg))
	}

	// check for spam with classifier if classifier is loaded
	if d.classifier.nAllDocument > 0 {
		cr = append(cr, d.isSpamClassified(msg))
	}

	// check for spam with CAS API if CAS API URL is set
	if d.CasAPI != "" {
		cr = append(cr, d.isCasSpam(userID))
	}

	spamDetected := isSpamDetected(cr)

	// we hit openai in two cases:
	//  - all other checks passed (ham result) and OpenAIVeto is false. In this case, openai primary used to improve false negative rate
	//  - one of the checks failed (spam result) and OpenAIVeto is true. In this case, openai primary used to improve false positive rate
	// FirstMessageOnly or FirstMessagesCount has to be set to use openai, because it's slow and expensive to run on all messages
	if d.openaiChecker != nil && (d.FirstMessageOnly || d.FirstMessagesCount > 0) {
		if !spamDetected && !d.OpenAIVeto || spamDetected && d.OpenAIVeto {
			spam, details := d.openaiChecker.check(msg)
			cr = append(cr, details)
			spamDetected = spam
		}
	}

	if spamDetected {
		return true, cr
	}

	if d.FirstMessageOnly || d.FirstMessagesCount > 0 {
		d.approvedUsers[userID]++
	}
	return false, cr
}

// Reset resets spam samples/classifier, excluded tokens, stop words and approved users.
func (d *Detector) Reset() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.tokenizedSpam = []map[string]int{}
	d.excludedTokens = []string{}
	d.classifier.reset()
	d.approvedUsers = make(map[string]int)
	d.stopWords = []string{}
}

// WithSpamUpdater sets a SampleUpdater for spam samples.
func (d *Detector) WithSpamUpdater(s SampleUpdater) { d.spamSamplesUpd = s }

// WithHamUpdater sets a SampleUpdater for ham samples.
func (d *Detector) WithHamUpdater(s SampleUpdater) { d.hamSamplesUpd = s }

// AddApprovedUsers adds user IDs to the list of approved users.
func (d *Detector) AddApprovedUsers(ids ...string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	for _, id := range ids {
		d.approvedUsers[id] = d.FirstMessagesCount + 1 // +1 to skip first message check if count is 0
	}
}

// RemoveApprovedUsers removes user IDs from the list of approved users.
func (d *Detector) RemoveApprovedUsers(ids ...string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	for _, id := range ids {
		delete(d.approvedUsers, id)
	}
}

// LoadSamples loads spam samples from a reader and updates the classifier.
// Reset spam, ham samples/classifier, and excluded tokens.
func (d *Detector) LoadSamples(exclReader io.Reader, spamReaders, hamReaders []io.Reader) (LoadResult, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.tokenizedSpam = []map[string]int{}
	d.excludedTokens = []string{}
	d.classifier.reset()

	// excluded tokens should be loaded before spam samples to exclude them from spam tokenization
	for t := range d.tokenChan(exclReader) {
		d.excludedTokens = append(d.excludedTokens, strings.ToLower(t))
	}
	lr := LoadResult{ExcludedTokens: len(d.excludedTokens)}

	// load spam samples and update the classifier with them
	docs := []document{}
	for token := range d.tokenChan(spamReaders...) {
		tokenizedSpam := d.tokenize(token)
		d.tokenizedSpam = append(d.tokenizedSpam, tokenizedSpam) // add to list of samples
		tokens := make([]string, 0, len(tokenizedSpam))
		for token := range tokenizedSpam {
			tokens = append(tokens, token)
		}
		docs = append(docs, document{spamClass: "spam", tokens: tokens})
		lr.SpamSamples++
	}

	// load ham samples and update the classifier with them
	for token := range d.tokenChan(hamReaders...) {
		tokenizedSpam := d.tokenize(token)
		tokens := make([]string, 0, len(tokenizedSpam))
		for token := range tokenizedSpam {
			tokens = append(tokens, token)
		}
		docs = append(docs, document{spamClass: "ham", tokens: tokens})
		lr.HamSamples++
	}

	d.classifier.learn(docs...)
	return lr, nil
}

// LoadStopWords loads stop words from a reader. Reset stop words list before loading.
func (d *Detector) LoadStopWords(readers ...io.Reader) (LoadResult, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.stopWords = []string{}
	for t := range d.tokenChan(readers...) {
		d.stopWords = append(d.stopWords, strings.ToLower(t))
	}
	log.Printf("[INFO] loaded %d stop words", len(d.stopWords))
	return LoadResult{StopWords: len(d.stopWords)}, nil
}

// UpdateSpam appends a message to the spam samples file and updates the classifier
func (d *Detector) UpdateSpam(msg string) error { return d.updateSample(msg, d.spamSamplesUpd, "spam") }

// UpdateHam appends a message to the ham samples file and updates the classifier
func (d *Detector) UpdateHam(msg string) error { return d.updateSample(msg, d.hamSamplesUpd, "ham") }

// ApprovedUsers returns a list of approved users.
func (d *Detector) ApprovedUsers() (res []string) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	res = make([]string, 0, len(d.approvedUsers))
	for userID := range d.approvedUsers {
		res = append(res, userID)
	}
	return res
}

// LoadApprovedUsers loads a list of approved users from a reader.
// reset approved users list before loading. It expects a list of user IDs (int64) from the reader, one per line.
func (d *Detector) LoadApprovedUsers(r io.Reader) (count int, err error) {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.approvedUsers = make(map[string]int)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		userID := scanner.Text()
		if userID == "" {
			continue
		}
		d.approvedUsers[userID] = d.FirstMessagesCount + 1
		count++
	}

	return count, scanner.Err()
}

// updateSample appends a message to the samples file and updates the classifier
// doesn't reset state, update append samples
func (d *Detector) updateSample(msg string, upd SampleUpdater, sc spamClass) error {
	d.lock.Lock()
	defer d.lock.Unlock()

	if upd == nil {
		return nil
	}

	// write to dynamic samples storage
	if err := upd.Append(msg); err != nil {
		return fmt.Errorf("can't update %s samples: %w", sc, err)
	}

	// load samples and update the classifier with them
	docs := []document{}
	for token := range d.tokenChan(bytes.NewBufferString(msg)) {
		tokenizedSample := d.tokenize(token)
		tokens := make([]string, 0, len(tokenizedSample))
		for token := range tokenizedSample {
			tokens = append(tokens, token)
		}
		docs = append(docs, document{spamClass: sc, tokens: tokens})
	}
	d.classifier.learn(docs...)
	return nil
}

// tokenChan parses readers and returns a channel of tokens.
// A line per-token or comma-separated "tokens" supported
func (d *Detector) tokenChan(readers ...io.Reader) <-chan string {
	resCh := make(chan string)

	go func() {
		defer close(resCh)

		for _, reader := range readers {
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, ",") && strings.HasPrefix(line, "\"") {
					// line with comma-separated tokens
					lineTokens := strings.Split(line, ",")
					for _, token := range lineTokens {
						cleanToken := strings.Trim(token, " \"\n\r\t")
						if cleanToken != "" {
							resCh <- cleanToken
						}
					}
					continue
				}
				// each line with a single token
				cleanToken := strings.Trim(line, " \n\r\t")
				if cleanToken != "" {
					resCh <- cleanToken
				}
			}

			if err := scanner.Err(); err != nil {
				log.Printf("[WARN] failed to read tokens, error=%v", err)
			}
		}
	}()

	return resCh
}

// tokenize takes a string and returns a map where the keys are unique words (tokens)
// and the values are the frequencies of those words in the string.
// exclude tokens representing common words.
func (d *Detector) tokenize(inp string) map[string]int {
	isExcludedToken := func(token string) bool {
		for _, w := range d.excludedTokens {
			if strings.EqualFold(token, w) {
				return true
			}
		}
		return false
	}

	tokenFrequency := make(map[string]int)
	tokens := strings.Fields(inp)
	for _, token := range tokens {
		if isExcludedToken(token) {
			continue
		}
		token = cleanEmoji(token)
		token = strings.Trim(token, ".,!?-:;()#")
		token = strings.ToLower(token)
		if len([]rune(token)) < 3 {
			continue
		}
		tokenFrequency[strings.ToLower(token)]++
	}
	return tokenFrequency
}

// isSpam checks if a given message is similar to any of the known bad messages
func (d *Detector) isSpamSimilarityHigh(msg string) CheckResult {
	// check for spam similarity
	tokenizedMessage := d.tokenize(msg)
	maxSimilarity := 0.0
	for _, spam := range d.tokenizedSpam {
		similarity := d.cosineSimilarity(tokenizedMessage, spam)
		if similarity > maxSimilarity {
			maxSimilarity = similarity
		}
		if similarity >= d.SimilarityThreshold {
			return CheckResult{Spam: true, Name: "similarity",
				Details: fmt.Sprintf("%0.2f/%0.2f", maxSimilarity, d.SimilarityThreshold)}
		}
	}
	return CheckResult{Spam: false, Name: "similarity", Details: fmt.Sprintf("%0.2f/%0.2f", maxSimilarity, d.SimilarityThreshold)}
}

// cosineSimilarity calculates the cosine similarity between two token frequency maps.
func (d *Detector) cosineSimilarity(a, b map[string]int) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}

	dotProduct := 0      // sum of product of corresponding frequencies
	normA, normB := 0, 0 // square root of sum of squares of frequencies

	for key, val := range a {
		dotProduct += val * b[key]
		normA += val * val
	}
	for _, val := range b {
		normB += val * val
	}

	if normA == 0 || normB == 0 {
		return 0.0
	}

	// cosine similarity formula
	return float64(dotProduct) / (math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
}

// isCasSpam checks if a given user ID is a spammer with CAS API.
func (d *Detector) isCasSpam(msgID string) CheckResult {
	if _, err := strconv.ParseInt(msgID, 10, 64); err != nil {
		return CheckResult{Spam: false, Name: "cas", Details: fmt.Sprintf("invalid user id %q", msgID)}
	}
	reqURL := fmt.Sprintf("%s/check?user_id=%s", d.CasAPI, msgID)
	req, err := http.NewRequest("GET", reqURL, http.NoBody)
	if err != nil {
		return CheckResult{Spam: false, Name: "cas", Details: fmt.Sprintf("failed to make request %s: %v", reqURL, err)}
	}

	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return CheckResult{Spam: false, Name: "cas", Details: fmt.Sprintf("ffailed to send request %s: %v", reqURL, err)}
	}
	defer resp.Body.Close()

	respData := struct {
		OK          bool   `json:"ok"` // ok means user is a spammer
		Description string `json:"description"`
	}{}

	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return CheckResult{Spam: false, Name: "cas", Details: fmt.Sprintf("failed to parse response from %s: %v", reqURL, err)}
	}
	respData.Description = strings.ToLower(respData.Description)
	respData.Description = strings.TrimSuffix(respData.Description, ".")

	if respData.OK {
		return CheckResult{Name: "cas", Spam: true, Details: respData.Description}
	}
	details := respData.Description
	if details == "" {
		details = "not found"
	}
	return CheckResult{Name: "cas", Spam: false, Details: details}
}

// isSpamClassified classify tokens from a document
func (d *Detector) isSpamClassified(msg string) CheckResult {
	tm := d.tokenize(msg)
	tokens := make([]string, 0, len(tm))
	for token := range tm {
		tokens = append(tokens, token)
	}
	class, prob, certain := d.classifier.classify(tokens...)
	isSpam := class == "spam" && certain && (d.MinSpamProbability == 0 || prob >= d.MinSpamProbability)
	return CheckResult{Name: "classifier", Spam: isSpam,
		Details: fmt.Sprintf("probability of %s: %.2f%%", class, prob)}
}

// isStopWord checks if a given message contains any of the stop words.
func (d *Detector) isStopWord(msg string) CheckResult {
	cleanMsg := cleanEmoji(strings.ToLower(msg))
	for _, word := range d.stopWords { // stop words are already lowercased
		if strings.Contains(cleanMsg, strings.ToLower(word)) {
			return CheckResult{Name: "stopword", Spam: true, Details: word}
		}
	}
	return CheckResult{Name: "stopword", Spam: false, Details: "not found"}
}

// isManyEmojis checks if a given message contains more than MaxAllowedEmoji emojis.
func (d *Detector) isManyEmojis(msg string) CheckResult {
	count := countEmoji(msg)
	return CheckResult{Name: "emoji", Spam: count > d.MaxAllowedEmoji, Details: fmt.Sprintf("%d/%d", count, d.MaxAllowedEmoji)}
}

func (c *CheckResult) String() string {
	spamOrHam := "ham"
	if c.Spam {
		spamOrHam = "spam"
	}
	return fmt.Sprintf("%s: %s, %s", c.Name, spamOrHam, c.Details)
}
