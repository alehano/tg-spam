package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/go-pkgz/lgr"
	tbapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
	"github.com/umputun/go-flags"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/webapi"
	"github.com/umputun/tg-spam/lib"
)

type options struct {
	Telegram struct {
		Token            string        `long:"token" env:"TOKEN" description:"telegram bot token"`
		Group            string        `long:"group" env:"GROUP" description:"group name/id"`
		Timeout          time.Duration `long:"timeout" env:"TIMEOUT" default:"30s" description:"http client timeout for telegram" `
		IdleDuration     time.Duration `long:"idle" env:"IDLE" default:"30s" description:"idle duration"`
		PreserveUnbanned bool          `long:"preserve-unbanned" env:"PRESERVE_UNBANNED" description:"preserve user after unban"`
	} `group:"telegram" namespace:"telegram" env-namespace:"TELEGRAM"`

	AdminGroup string  `long:"admin.group" env:"ADMIN_GROUP" description:"admin group name, or channel id"`
	TestingIDs []int64 `long:"testing-id" env:"TESTING_ID" env-delim:"," description:"testing ids, allow bot to reply to them"`

	HistoryDuration time.Duration `long:"history-duration" env:"HISTORY_DURATION" default:"24h" description:"history duration"`
	HistoryMinSize  int           `long:"history-min-size" env:"HISTORY_MIN_SIZE" default:"1000" description:"history minimal size to keep"`

	Logger struct {
		Enabled    bool   `long:"enabled" env:"ENABLED" description:"enable spam rotated logs"`
		FileName   string `long:"file" env:"FILE"  default:"tg-spam.log" description:"location of spam log"`
		MaxSize    string `long:"max-size" env:"MAX_SIZE" default:"100M" description:"maximum size before it gets rotated"`
		MaxBackups int    `long:"max-backups" env:"MAX_BACKUPS" default:"10" description:"maximum number of old log files to retain"`
	} `group:"logger" namespace:"logger" env-namespace:"LOGGER"`

	SuperUsers  events.SuperUsers `long:"super" env:"SUPER_USER" env-delim:"," description:"super-users"`
	NoSpamReply bool              `long:"no-spam-reply" env:"NO_SPAM_REPLY" description:"do not reply to spam messages"`

	CAS struct {
		API     string        `long:"api" env:"API" default:"https://api.cas.chat" description:"CAS API"`
		Timeout time.Duration `long:"timeout" env:"TIMEOUT" default:"5s" description:"CAS timeout"`
	} `group:"cas" namespace:"cas" env-namespace:"CAS"`

	OpenAI struct {
		Token                            string `long:"token" env:"TOKEN" description:"openai token, disabled if not set"`
		Veto                             bool   `long:"veto" env:"VETO" description:"veto mode, confirm detected spam"`
		Prompt                           string `long:"prompt" env:"PROMPT" default:"" description:"openai system prompt, if empty uses builtin default"`
		Model                            string `long:"model" env:"MODEL" default:"gpt-4" description:"openai model"`
		MaxTokensResponse                int    `long:"max-tokens-response" env:"MAX_TOKENS_RESPONSE" default:"1024" description:"openai max tokens in response"`
		MaxTokensRequestMaxTokensRequest int    `long:"max-tokens-request" env:"MAX_TOKENS_REQUEST" default:"2048" description:"openai max tokens in request"`
		MaxSymbolsRequest                int    `long:"max-symbols-request" env:"MAX_SYMBOLS_REQUEST" default:"16000" description:"openai max symbols in request, failback if tokenizer failed"`
	} `group:"openai" namespace:"openai" env-namespace:"OPENAI"`

	Files struct {
		SamplesDataPath string        `long:"samples" env:"SAMPLES" default:"data" description:"samples data path"`
		DynamicDataPath string        `long:"dynamic" env:"DYNAMIC" default:"data" description:"dynamic data path"`
		WatchInterval   time.Duration `long:"watch-interval" env:"WATCH_INTERVAL" default:"5s" description:"watch interval for dynamic files"`
	} `group:"files" namespace:"files" env-namespace:"FILES"`

	SimilarityThreshold float64 `long:"similarity-threshold" env:"SIMILARITY_THRESHOLD" default:"0.5" description:"spam threshold"`
	MinMsgLen           int     `long:"min-msg-len" env:"MIN_MSG_LEN" default:"50" description:"min message length to check"`
	MaxEmoji            int     `long:"max-emoji" env:"MAX_EMOJI" default:"2" description:"max emoji count in message, -1 to disable check"`
	MinSpamProbability  float64 `long:"min-probability" env:"MIN_PROBABILITY" default:"50" description:"min spam probability percent to ban"`

	ParanoidMode       bool `long:"paranoid" env:"PARANOID" description:"paranoid mode, check all messages"`
	FirstMessagesCount int  `long:"first-messages-count" env:"FIRST_MESSAGES_COUNT" default:"1" description:"number of first messages to check"`

	Message struct {
		Startup string `long:"startup" env:"STARTUP" default:"" description:"startup message"`
		Spam    string `long:"spam" env:"SPAM" default:"this is spam" description:"spam message"`
		Dry     string `long:"dry" env:"DRY" default:"this is spam (dry mode)" description:"spam dry message"`
	} `group:"message" namespace:"message" env-namespace:"MESSAGE"`

	Server struct {
		Enabled    bool   `long:"enabled" env:"ENABLED" description:"enable web server"`
		ListenAddr string `long:"listen" env:"LISTEN" default:":8080" description:"listen address"`
		AuthPasswd string `long:"auth" env:"AUTH" default:"auto" description:"basic auth password for user 'tg-spam'"`
	} `group:"server" namespace:"server" env-namespace:"SERVER"`

	Training bool `long:"training" env:"TRAINING" description:"training mode, passive spam detection only"`
	Dry      bool `long:"dry" env:"DRY" description:"dry mode, no bans"`
	Dbg      bool `long:"dbg" env:"DEBUG" description:"debug mode"`
	TGDbg    bool `long:"tg-dbg" env:"TG_DEBUG" description:"telegram debug mode"`
}

// file names
const (
	samplesSpamFile   = "spam-samples.txt"
	samplesHamFile    = "ham-samples.txt"
	excludeTokensFile = "exclude-tokens.txt"
	stopWordsFile     = "stop-words.txt" //nolint:gosec // false positive
	dynamicSpamFile   = "spam-dynamic.txt"
	dynamicHamFile    = "ham-dynamic.txt"
	dataFile          = "tg-spam.db"
)

var revision = "local"

func main() {
	fmt.Printf("tg-spam %s\n", revision)
	var opts options
	p := flags.NewParser(&opts, flags.PrintErrors|flags.PassDoubleDash|flags.HelpFlag)
	p.SubcommandsOptional = true
	if _, err := p.Parse(); err != nil {
		if err.(*flags.Error).Type != flags.ErrHelp {
			log.Printf("[ERROR] cli error: %v", err)
		}
		os.Exit(2)
	}

	setupLog(opts.Dbg, opts.Telegram.Token, opts.OpenAI.Token)
	log.Printf("[DEBUG] options: %+v", opts)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// catch signal and invoke graceful termination
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		log.Printf("[WARN] interrupt signal")
		cancel()
	}()

	opts.Files.DynamicDataPath = expandPath(opts.Files.DynamicDataPath)
	opts.Files.SamplesDataPath = expandPath(opts.Files.SamplesDataPath)

	if err := execute(ctx, opts); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, opts options) error {
	if opts.Dry {
		log.Print("[WARN] dry mode, no actual bans")
	}

	if !opts.Server.Enabled && (opts.Telegram.Token == "" || opts.Telegram.Group == "") {
		return errors.New("telegram token and group are required")
	}

	checkVolumeMount(opts)

	// make samples and dynamic data dirs
	if err := os.MkdirAll(opts.Files.SamplesDataPath, 0o700); err != nil {
		return fmt.Errorf("can't make samples dir, %w", err)
	}
	if err := os.MkdirAll(opts.Files.DynamicDataPath, 0o700); err != nil {
		return fmt.Errorf("can't make dynamic dir, %w", err)
	}

	// make detector with all sample files loaded
	detector := makeDetector(opts)

	dataFile := filepath.Join(opts.Files.DynamicDataPath, dataFile)
	dataDB, err := storage.NewSqliteDB(dataFile)
	if err != nil {
		return fmt.Errorf("can't make data db file %s, %w", dataFile, err)
	}
	log.Printf("[DEBUG] data db: %s", dataFile)

	// load approved users
	approvedUsersStore, auErr := storage.NewApprovedUsers(dataDB)
	if auErr != nil {
		return fmt.Errorf("can't make approved users store, %w", auErr)
	}
	defer func() {
		if serr := approvedUsersStore.Store(detector.ApprovedUsers()); serr != nil {
			log.Printf("[WARN] can't save approved users, %v", serr)
		}
	}()
	count, lerr := detector.LoadApprovedUsers(approvedUsersStore)
	if lerr != nil {
		log.Printf("[WARN] can't load approved users, %v", lerr)
	} else {
		log.Printf("[DEBUG] approved users from: %s, loaded: %d", dataFile, count)
	}

	// make spam bot
	spamBot, err := makeSpamBot(ctx, opts, detector)
	if err != nil {
		return fmt.Errorf("can't make spam bot, %w", err)
	}

	// activate web server if enabled
	if opts.Server.Enabled {
		// server starts in background goroutine
		if srvErr := activateServer(ctx, opts, spamBot); srvErr != nil {
			return fmt.Errorf("can't activate web server, %w", srvErr)
		}
		if opts.Telegram.Token == "" || opts.Telegram.Group == "" {
			log.Printf("[WARN] no telegram token and group, web server only mode")
			// if no telegram token and group set, just run the server
			<-ctx.Done()
			return nil
		}
	}

	// make telegram bot
	tbAPI, err := tbapi.NewBotAPI(opts.Telegram.Token)
	if err != nil {
		return fmt.Errorf("can't make telegram bot, %w", err)
	}
	tbAPI.Debug = opts.TGDbg

	go autoSaveApprovedUsers(ctx, detector, approvedUsersStore, time.Minute*5)

	// make spam logger
	loggerWr, err := makeSpamLogWriter(opts)
	if err != nil {
		return fmt.Errorf("can't make spam log writer, %w", err)
	}
	defer loggerWr.Close()

	locator, err := storage.NewLocator(opts.HistoryDuration, opts.HistoryMinSize, dataDB)
	if err != nil {
		return fmt.Errorf("can't make locator, %w", err)
	}

	// make telegram listener
	tgListener := events.TelegramListener{
		TbAPI:        tbAPI,
		Group:        opts.Telegram.Group,
		IdleDuration: opts.Telegram.IdleDuration,
		SuperUsers:   opts.SuperUsers,
		Bot:          spamBot,
		StartupMsg:   opts.Message.Startup,
		NoSpamReply:  opts.NoSpamReply,
		SpamLogger:   makeSpamLogger(loggerWr),
		AdminGroup:   opts.AdminGroup,
		TestingIDs:   opts.TestingIDs,
		Locator:      locator,
		TrainingMode: opts.Training,
		Dry:          opts.Dry,
		KeepUser:     opts.Telegram.PreserveUnbanned,
	}
	log.Printf("[DEBUG] telegram listener config: {group: %s, idle: %v, super: %v, admin: %s, testing: %v, no-reply: %v,"+
		" dry: %v, training: %v, preserve-unbanned: %v}",
		tgListener.Group, tgListener.IdleDuration, tgListener.SuperUsers, tgListener.AdminGroup,
		tgListener.TestingIDs, tgListener.NoSpamReply, tgListener.Dry, tgListener.TrainingMode, tgListener.KeepUser)

	// run telegram listener and event processor loop
	if err := tgListener.Do(ctx); err != nil {
		return fmt.Errorf("telegram listener failed, %w", err)
	}
	return nil
}

// checkVolumeMount checks if dynamic files location mounted in docker and shows warning if not
// returns true if running not in docker or dynamic files dir mounted
func checkVolumeMount(opts options) (ok bool) {
	if os.Getenv("TGSPAM_IN_DOCKER") != "1" {
		return true
	}
	log.Printf("[DEBUG] running in docker")
	warnMsg := fmt.Sprintf("dynamic files dir %q is not mounted, changes will be lost on container restart", opts.Files.DynamicDataPath)

	// check if dynamic files dir not present. This means it is not mounted
	_, err := os.Stat(opts.Files.DynamicDataPath)
	if err != nil {
		log.Printf("[WARN] %s", warnMsg)
		// no dynamic files dir, no need to check further
		return false
	}

	// check if .not_mounted file missing, this means it is mounted
	if _, err = os.Stat(filepath.Join(opts.Files.DynamicDataPath, ".not_mounted")); err != nil {
		return true
	}

	// if .not_mounted file present, it can be mounted anyway with docker named volumes
	output, err := exec.Command("mount").Output()
	if err != nil {
		log.Printf("[WARN] %s, can't check mount, %v", warnMsg, err)
		return true
	}
	// check if the output contains the specified directory
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, opts.Files.DynamicDataPath) {
			return true
		}
	}

	log.Printf("[WARN] %s", warnMsg)
	return false
}

func activateServer(ctx context.Context, opts options, spamFilter *bot.SpamFilter) (err error) {
	authPassswd := opts.Server.AuthPasswd
	if opts.Server.AuthPasswd == "auto" {
		authPassswd, err = webapi.GenerateRandomPassword(20)
		if err != nil {
			return fmt.Errorf("can't generate random password, %w", err)
		}
		log.Printf("[WARN] generated basic auth password for user tg-spam: %q", authPassswd)
	}

	srv := webapi.Server{Config: webapi.Config{
		ListenAddr: opts.Server.ListenAddr,
		SpamFilter: spamFilter.Detector,
		AuthPasswd: authPassswd,
		Version:    revision,
		Dbg:        opts.Dbg,
	}}

	go func() {
		if err := srv.Run(ctx); err != nil {
			log.Printf("[ERROR] web server failed, %v", err)
		}
	}()
	return nil
}

// makeDetector creates spam detector with all checkers and updaters
// it loads samples and dynamic files
func makeDetector(opts options) *lib.Detector {
	detectorConfig := lib.Config{
		MaxAllowedEmoji:     opts.MaxEmoji,
		MinMsgLen:           opts.MinMsgLen,
		SimilarityThreshold: opts.SimilarityThreshold,
		MinSpamProbability:  opts.MinSpamProbability,
		CasAPI:              opts.CAS.API,
		HTTPClient:          &http.Client{Timeout: opts.CAS.Timeout},
		FirstMessageOnly:    !opts.ParanoidMode,
		FirstMessagesCount:  opts.FirstMessagesCount,
		OpenAIVeto:          opts.OpenAI.Veto,
	}

	// FirstMessagesCount and ParanoidMode are mutually exclusive.
	// ParanoidMode still here for backward compatibility only.
	if opts.FirstMessagesCount > 0 { // if FirstMessagesCount is set, FirstMessageOnly is enforced
		detectorConfig.FirstMessageOnly = true
	}
	if opts.ParanoidMode { // if ParanoidMode is set, FirstMessagesCount is ignored
		detectorConfig.FirstMessageOnly = false
		detectorConfig.FirstMessagesCount = 0
	}

	detector := lib.NewDetector(detectorConfig)
	log.Printf("[DEBUG] detector config: %+v", detectorConfig)

	if opts.OpenAI.Token != "" {
		log.Printf("[WARN] openai enabled")
		openAIConfig := lib.OpenAIConfig{
			SystemPrompt:      opts.OpenAI.Prompt,
			Model:             opts.OpenAI.Model,
			MaxTokensResponse: opts.OpenAI.MaxTokensResponse,
			MaxTokensRequest:  opts.OpenAI.MaxTokensRequestMaxTokensRequest,
			MaxSymbolsRequest: opts.OpenAI.MaxSymbolsRequest,
		}
		log.Printf("[DEBUG] openai  config: %+v", openAIConfig)
		detector.WithOpenAIChecker(openai.NewClient(opts.OpenAI.Token), openAIConfig)
	}

	dynSpamFile := filepath.Join(opts.Files.DynamicDataPath, dynamicSpamFile)
	detector.WithSpamUpdater(bot.NewSampleUpdater(dynSpamFile))
	log.Printf("[DEBUG] dynamic spam file: %s", dynSpamFile)

	dynHamFile := filepath.Join(opts.Files.DynamicDataPath, dynamicHamFile)
	detector.WithHamUpdater(bot.NewSampleUpdater(dynHamFile))
	log.Printf("[DEBUG] dynamic ham file: %s", dynHamFile)

	return detector
}

func makeSpamBot(ctx context.Context, opts options, detector *lib.Detector) (*bot.SpamFilter, error) {
	spamBotParams := bot.SpamConfig{
		SpamSamplesFile:    filepath.Join(opts.Files.SamplesDataPath, samplesSpamFile),
		HamSamplesFile:     filepath.Join(opts.Files.SamplesDataPath, samplesHamFile),
		StopWordsFile:      filepath.Join(opts.Files.SamplesDataPath, stopWordsFile),
		ExcludedTokensFile: filepath.Join(opts.Files.SamplesDataPath, excludeTokensFile),
		SpamDynamicFile:    filepath.Join(opts.Files.DynamicDataPath, dynamicSpamFile),
		HamDynamicFile:     filepath.Join(opts.Files.DynamicDataPath, dynamicHamFile),
		WatchDelay:         opts.Files.WatchInterval,
		SpamMsg:            opts.Message.Spam,
		SpamDryMsg:         opts.Message.Dry,
		Dry:                opts.Dry,
	}
	spamBot := bot.NewSpamFilter(ctx, detector, spamBotParams)
	log.Printf("[DEBUG] spam bot config: %+v", spamBotParams)

	if err := spamBot.ReloadSamples(); err != nil {
		return nil, fmt.Errorf("can't relaod samples, %w", err)
	}
	return spamBot, nil
}

// makeSpamLogger creates spam logger to keep reports about spam messages
// it writes json lines to the provided writer
func makeSpamLogger(wr io.Writer) events.SpamLogger {
	return events.SpamLoggerFunc(func(msg *bot.Message, response *bot.Response) {
		text := strings.ReplaceAll(msg.Text, "\n", " ")
		text = strings.TrimSpace(text)
		log.Printf("[DEBUG] spam detected from %v, text: %s", msg.From, text)
		m := struct {
			TimeStamp   string `json:"ts"`
			DisplayName string `json:"display_name"`
			UserName    string `json:"user_name"`
			UserID      int64  `json:"user_id"`
			Text        string `json:"text"`
		}{
			TimeStamp:   time.Now().In(time.Local).Format(time.RFC3339),
			DisplayName: msg.From.DisplayName,
			UserName:    msg.From.Username,
			UserID:      msg.From.ID,
			Text:        text,
		}
		line, err := json.Marshal(&m)
		if err != nil {
			log.Printf("[WARN] can't marshal json, %v", err)
			return
		}
		if _, err := wr.Write(append(line, '\n')); err != nil {
			log.Printf("[WARN] can't write to log, %v", err)
		}
	})
}

// makeSpamLogWriter creates spam log writer to keep reports about spam messages
// it parses options and makes lumberjack logger with rotation
func makeSpamLogWriter(opts options) (accessLog io.WriteCloser, err error) {
	if !opts.Logger.Enabled {
		return nopWriteCloser{io.Discard}, nil
	}

	sizeParse := func(inp string) (uint64, error) {
		if inp == "" {
			return 0, errors.New("empty value")
		}
		for i, sfx := range []string{"k", "m", "g", "t"} {
			if strings.HasSuffix(inp, strings.ToUpper(sfx)) || strings.HasSuffix(inp, strings.ToLower(sfx)) {
				val, err := strconv.Atoi(inp[:len(inp)-1])
				if err != nil {
					return 0, fmt.Errorf("can't parse %s: %w", inp, err)
				}
				return uint64(float64(val) * math.Pow(float64(1024), float64(i+1))), nil
			}
		}
		return strconv.ParseUint(inp, 10, 64)
	}

	maxSize, perr := sizeParse(opts.Logger.MaxSize)
	if perr != nil {
		return nil, fmt.Errorf("can't parse logger MaxSize: %w", perr)
	}

	maxSize /= 1048576

	log.Printf("[INFO] logger enabled for %s, max size %dM", opts.Logger.FileName, maxSize)
	return &lumberjack.Logger{
		Filename:   opts.Logger.FileName,
		MaxSize:    int(maxSize), // in MB
		MaxBackups: opts.Logger.MaxBackups,
		Compress:   true,
		LocalTime:  true,
	}, nil
}

func autoSaveApprovedUsers(ctx context.Context, detector *lib.Detector, store *storage.ApprovedUsers, interval time.Duration) {
	log.Printf("[DEBUG] auto-save approved users every %v", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastCount := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("[DEBUG] auto-save approved users stopped")
			return
		case <-ticker.C:
			ids := detector.ApprovedUsers()
			if len(ids) == lastCount {
				continue
			}
			if err := store.Store(ids); err != nil {
				log.Printf("[WARN] can't save approved users, %v", err)
				continue
			}
			lastCount = len(ids)
		}
	}
}

func expandPath(path string) string {
	if path == "" {
		return ""
	}
	if path[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, path[1:])
	}
	ep, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return ep
}

type nopWriteCloser struct{ io.Writer }

func (n nopWriteCloser) Close() error { return nil }

func setupLog(dbg bool, secrets ...string) {
	logOpts := []lgr.Option{lgr.Msec, lgr.LevelBraces, lgr.StackTraceOnError}
	if dbg {
		logOpts = []lgr.Option{lgr.Debug, lgr.CallerFile, lgr.CallerFunc, lgr.Msec, lgr.LevelBraces, lgr.StackTraceOnError}
	}

	colorizer := lgr.Mapper{
		ErrorFunc:  func(s string) string { return color.New(color.FgHiRed).Sprint(s) },
		WarnFunc:   func(s string) string { return color.New(color.FgRed).Sprint(s) },
		InfoFunc:   func(s string) string { return color.New(color.FgYellow).Sprint(s) },
		DebugFunc:  func(s string) string { return color.New(color.FgWhite).Sprint(s) },
		CallerFunc: func(s string) string { return color.New(color.FgBlue).Sprint(s) },
		TimeFunc:   func(s string) string { return color.New(color.FgCyan).Sprint(s) },
	}
	logOpts = append(logOpts, lgr.Map(colorizer))

	if len(secrets) > 0 {
		logOpts = append(logOpts, lgr.Secret(secrets...))
	}
	lgr.SetupStdLogger(logOpts...)
	lgr.Setup(logOpts...)
}
