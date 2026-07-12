package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
	"github.com/mguyenanastacio-glitch/pan-fetcher/indexer"
	"github.com/mguyenanastacio-glitch/pan-fetcher/p115"
	"github.com/mguyenanastacio-glitch/pan-fetcher/rsssite"
	"github.com/mguyenanastacio-glitch/pan-fetcher/server"
)

var (
	pAgent        *p115.Agent
	rssUrl        string
	cookies       string
	rssJsonPath   string
	qrLogin       bool
	disableCache  bool
	chunkDelay    int
	chunkSize     int
	cooldownMinMs int
	cooldownMaxMs int
	clearTaskNum  int
	rootCmd       = &cobra.Command{
		Use:   "pan-fetcher",
		Short: `Add offline tasks to 115`,
		Run: func(_cmd *cobra.Command, _args []string) {
			initAgent(_cmd)
			if rssJsonPath != "" {
				rsssite.SetRssJsonPath(rssJsonPath)
			}
			if rssUrl != "" {
				pAgent.AddRssUrlTask(rssUrl)
				return
			}
			if clearTaskNum > 0 {
				err := pAgent.OfflineClear(clearTaskNum - 1)
				if err != nil {
					log.Fatalln(err)
				}
				return
			}
			pAgent.ExecuteAllRssTask()
		},
	}
	// magnet link
	linkUrl   string
	cid       string
	savepath  string
	textFile  string
	magnetCmd = &cobra.Command{
		Use:   "magnet",
		Short: `Add magnet tasks to 115`,
		Run: func(_cmd *cobra.Command, _args []string) {
			initAgent(_cmd)
			magnets := []string{}
			if textFile != "" {
				var err error
				magnets, err = rsssite.GetMagnetsFromText(textFile)
				if err != nil {
					log.Fatalln(err)
				}
			} else if linkUrl != "" {
				magnets = append(magnets, linkUrl)
			}
			if len(magnets) == 0 {
				log.Fatalln("magnets is empty")
			}
			pAgent.AddMagnetTask(magnets, cid, savepath)
		},
	}
	// server subcommand
	port        int
	webPassword string

	serverCmd = &cobra.Command{
		Use:   "server",
		Short: `Start server`,
		Run: func(_cmd *cobra.Command, _args []string) {
			cfg := initAgent(_cmd)
			server.SetPassword(webPassword)
			srv := server.New(pAgent, cfg.Server.Port)
			srv.LoadProxyConfig()

			// Initialize indexer manager from YAML definitions
			idxDir := filepath.Join(filepath.Dir(cfg.Database.Path), "indexers")
			if mgr, err := indexer.NewManager(idxDir); err == nil {
				srv.SetIndexerManager(mgr)
				log.Printf("[indexer] loaded %d definitions, 0 active (from %s)", mgr.LibraryCount(), idxDir)
			}

			srv.StartServer()
		},
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&rssUrl, "url", "u", "", "rss url")
	rootCmd.PersistentFlags().StringVar(&cookies, "cookies", "", "115 cookies")
	rootCmd.PersistentFlags().StringVarP(&rssJsonPath, "rss", "r", "", "rss json path")
	rootCmd.PersistentFlags().BoolVarP(&qrLogin, "qrcode", "q", false, "login 115 by qrcode")
	magnetCmd.Flags().StringVarP(&linkUrl, "link", "l", "", "magnet link")
	magnetCmd.Flags().StringVar(&cid, "cid", "", "cid")
	magnetCmd.Flags().StringVar(&savepath, "savepath", "", "save path")
	magnetCmd.Flags().StringVar(&textFile, "text", "", "text file")
	rootCmd.PersistentFlags().BoolVar(&disableCache, "no-cache", false, "skip checking cache in db.sqlite")
	rootCmd.PersistentFlags().IntVar(&chunkDelay, "chunk-delay", 0, "chunk delay. default 2")
	rootCmd.PersistentFlags().IntVar(&chunkSize, "chunk-size", 0, "chunk size. default 200")
	rootCmd.PersistentFlags().IntVar(&cooldownMinMs, "cooldown-min-ms", 1000, "minimum cooldown between 115 API calls in milliseconds. default 1000")
	rootCmd.PersistentFlags().IntVar(&cooldownMaxMs, "cooldown-max-ms", 1100, "maximum cooldown between 115 API calls in milliseconds. default 1100")
	rootCmd.Flags().IntVar(&clearTaskNum, "clear-task-type", 0, "clear offline task type: 1-6.\n 1: OfflineClearDone\n 2: OfflineClearAll\n 3: OfflineClearFailed\n 4: OfflineClearRunning\n 5: OfflineClearDoneAndDelete\n 6: OfflineClearAllAndDelete")
	rootCmd.AddCommand(magnetCmd)
	// server subcommand
	serverCmd.Flags().IntVarP(&port, "port", "p", 8115, "server port")
	serverCmd.Flags().StringVar(&webPassword, "web-password", "", "web login password (empty = no auth)")
	rootCmd.AddCommand(serverCmd)
}

func buildCLIParams(cmd *cobra.Command) config.CLIParams {
	cliParams := config.CLIParams{
		Cookies: cookies,
	}

	if commandFlagChanged(cmd, "no-cache") {
		cliParams.DisableCache = disableCache
		cliParams.DisableCacheSet = true
	}
	if commandFlagChanged(cmd, "chunk-delay") {
		cliParams.ChunkDelay = chunkDelay
		cliParams.ChunkDelaySet = true
	}
	if commandFlagChanged(cmd, "chunk-size") {
		cliParams.ChunkSize = chunkSize
		cliParams.ChunkSizeSet = true
	}
	if commandFlagChanged(cmd, "cooldown-min-ms") {
		cliParams.CooldownMinMs = cooldownMinMs
		cliParams.CooldownMinMsSet = true
	}
	if commandFlagChanged(cmd, "cooldown-max-ms") {
		cliParams.CooldownMaxMs = cooldownMaxMs
		cliParams.CooldownMaxMsSet = true
	}
	if cmd != nil && cmd.Flags().Changed("port") {
		cliParams.Port = port
		cliParams.PortSet = true
	}
	return cliParams
}

func commandFlagChanged(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	return cmd.Flags().Changed(name) || cmd.InheritedFlags().Changed(name) || cmd.PersistentFlags().Changed(name)
}

func initAgent(cmd *cobra.Command) *config.Config {
	cliParams := buildCLIParams(cmd)

	cfg, _, err := config.LoadWithOptions(cliParams, config.LoadOptions{Auth: true})
	if err != nil {
		log.Fatalln(err)
	}

	p115.SetOption(p115.Option{
		DisableCache:  cfg.P115.DisableCache,
		ChunkDelay:    cfg.P115.ChunkDelay,
		ChunkSize:     cfg.P115.ChunkSize,
		CooldownMinMs: cfg.P115.CooldownMinMs,
		CooldownMaxMs: cfg.P115.CooldownMaxMs,
		DatabasePath:  cfg.Database.Path,
	})

	var agentErr error
	if qrLogin {
		pAgent, agentErr = p115.NewAgentByQrcode()
	} else if cfg.Auth.Cookies != "" {
		pAgent, agentErr = p115.NewAgent(cfg.Auth.Cookies)
	} else {
		pAgent, agentErr = p115.New()
	}
	if agentErr != nil {
		log.Printf("[server] 115 agent init skipped (no cookies): %v", agentErr)
		pAgent = nil
		agentErr = nil
	}
	return cfg
}
