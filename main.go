package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/application-research/estuary/autoretrieve"
	"github.com/application-research/estuary/build"
	"github.com/application-research/estuary/config"
	drpc "github.com/application-research/estuary/drpc"
	"github.com/application-research/estuary/metrics"
	"github.com/application-research/estuary/node"
	"github.com/application-research/estuary/pinner"
	"github.com/application-research/estuary/stagingbs"
	"github.com/application-research/estuary/util"
	"github.com/application-research/estuary/util/gateway"
	"github.com/application-research/filclient"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	gsimpl "github.com/ipfs/go-graphsync/impl"
	logging "github.com/ipfs/go-log/v2"
	routed "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/mitchellh/go-homedir"
	"github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/memo"
	"go.opencensus.io/stats/view"
	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/trace"
	"golang.org/x/xerrors"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/lotus/api"
	lcli "github.com/filecoin-project/lotus/cli"
	cli "github.com/urfave/cli/v2"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	if os.Getenv("FULLNODE_API_INFO") == "" {
		os.Setenv("FULLNODE_API_INFO", "wss://api.chain.love")
	}
}

var log = logging.Logger("estuary")

type storageMiner struct {
	gorm.Model
	Address         util.DbAddr `gorm:"unique"`
	Suspended       bool
	SuspendedReason string
	Name            string
	Version         string
	Location        string
	Owner           uint
}

func overrideSetOptions(flags []cli.Flag, cctx *cli.Context, cfg *config.Estuary) error {
	var err error
	for _, flag := range flags {
		name := flag.Names()[0]
		if cctx.IsSet(name) {
			log.Debugf("Flag %s is set to %s", name, cctx.String(name))
		} else {
			continue
		}
		switch name {
		case "datadir":
			cfg.SetDataDir(cctx.String("datadir"))
		case "blockstore":
			cfg.NodeConfig.BlockstoreDir, err = config.MakeAbsolute(cfg.DataDir, cctx.String("blockstore"))
		case "write-log-truncate":
			cfg.NodeConfig.WriteLogTruncate = cctx.Bool("write-log-truncate")
		case "write-log":
			cfg.NodeConfig.WriteLogDir, err = config.MakeAbsolute(cfg.DataDir, cctx.String("write-log"))
		case "database":
			cfg.DatabaseConnString = cctx.String("database")
		case "apilisten":
			cfg.ApiListen = cctx.String("apilisten")
		case "announce":
			_, err := multiaddr.NewMultiaddr(cctx.String("announce"))
			if err != nil {
				return fmt.Errorf("failed to parse announce address %s: %w", cctx.String("announce"), err)
			}
			cfg.NodeConfig.AnnounceAddrs = []string{cctx.String("announce")}
		case "lightstep-token":
			cfg.LightstepToken = cctx.String("lightstep-token")
		case "hostname":
			cfg.Hostname = cctx.String("hostname")
		case "default-replication":
			cfg.Replication = cctx.Int("default-replication")
		case "lowmem":
			cfg.LowMem = cctx.Bool("lowmem")
		case "no-storage-cron":
			cfg.DisableFilecoinStorage = cctx.Bool("no-storage-cron")
		case "disable-deal-making":
			cfg.DealConfig.Disable = cctx.Bool("disable-deal-making")
		case "fail-deals-on-transfer-failure":
			cfg.DealConfig.FailOnTransferFailure = cctx.Bool("fail-deals-on-transfer-failure")
		case "disable-local-content-adding":
			cfg.ContentConfig.DisableLocalAdding = cctx.Bool("disable-local-content-adding")
		case "disable-content-adding":
			cfg.ContentConfig.DisableGlobalAdding = cctx.Bool("disable-content-adding")
		case "jaeger-tracing":
			cfg.JaegerConfig.EnableTracing = cctx.Bool("jaeger-tracing")
		case "jaeger-provider-url":
			cfg.JaegerConfig.ProviderUrl = cctx.String("jaeger-provider-url")
		case "jaeger-sampler-ratio":
			cfg.JaegerConfig.SamplerRatio = cctx.Float64("jaeger-sampler-ratio")
		case "logging":
			cfg.LoggingConfig.ApiEndpointLogging = cctx.Bool("logging")
		default:
			// Do nothing
		}
		if (err) != nil {
			return err
		}
	}
	return err
}

func main() {
	logging.SetLogLevel("dt-impl", "debug")
	logging.SetLogLevel("estuary", "debug")
	logging.SetLogLevel("paych", "debug")
	logging.SetLogLevel("filclient", "debug")
	logging.SetLogLevel("dt_graphsync", "debug")
	//logging.SetLogLevel("graphsync_allocator", "debug")
	logging.SetLogLevel("dt-chanmon", "debug")
	logging.SetLogLevel("markets", "debug")
	logging.SetLogLevel("data_transfer_network", "debug")
	logging.SetLogLevel("rpc", "info")
	logging.SetLogLevel("bs-wal", "info")
	logging.SetLogLevel("provider.batched", "info")
	logging.SetLogLevel("bs-migrate", "info")
	logging.SetLogLevel("provider/engine", "debug")
	logging.SetLogLevel("chunker/cached-entries-chunker", "debug")

	app := cli.NewApp()
	home, _ := homedir.Dir()
	cfg := config.NewEstuary()

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
		},
		&cli.StringFlag{
			Name:  "config",
			Value: filepath.Join(home, ".estuary"),
			Usage: "specify configuration file location",
		},
		&cli.StringFlag{
			Name:    "database",
			Usage:   "specify connection string for estuary database",
			Value:   cfg.DatabaseConnString,
			EnvVars: []string{"ESTUARY_DATABASE"},
		},
		&cli.StringFlag{
			Name:    "apilisten",
			Usage:   "address for the api server to listen on",
			Value:   cfg.ApiListen,
			EnvVars: []string{"ESTUARY_API_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "announce",
			Usage:   "announce address for the libp2p server to listen on",
			EnvVars: []string{"ESTUARY_ANNOUNCE"},
		},
		&cli.StringFlag{
			Name:    "datadir",
			Usage:   "directory to store data in",
			Value:   cfg.DataDir,
			EnvVars: []string{"ESTUARY_DATADIR"},
		},
		&cli.StringFlag{
			Name:   "write-log",
			Usage:  "enable write log blockstore in specified directory",
			Value:  cfg.NodeConfig.WriteLogDir,
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:  "no-storage-cron",
			Usage: "run estuary without processing files into deals",
			Value: cfg.DisableFilecoinStorage,
		},
		&cli.BoolFlag{
			Name:  "logging",
			Usage: "enable api endpoint logging",
			Value: cfg.LoggingConfig.ApiEndpointLogging,
		},
		&cli.BoolFlag{
			Name:   "enable-auto-retrieve",
			Hidden: true,
			Value:  cfg.AutoRetrieve,
		},
		&cli.StringFlag{
			Name:    "lightstep-token",
			Usage:   "specify lightstep access token for enabling trace exports",
			EnvVars: []string{"ESTUARY_LIGHTSTEP_TOKEN"},
			Value:   cfg.LightstepToken,
		},
		&cli.StringFlag{
			Name:  "hostname",
			Usage: "specify hostname this node will be reachable at",
			Value: cfg.Hostname,
		},
		&cli.BoolFlag{
			Name:  "fail-deals-on-transfer-failure",
			Usage: "consider deals failed when the transfer to the miner fails",
			Value: cfg.DealConfig.FailOnTransferFailure,
		},
		&cli.BoolFlag{
			Name:  "disable-deal-making",
			Usage: "do not create any new deals (existing deals will still be processed)",
			Value: cfg.DealConfig.Disable,
		},
		&cli.BoolFlag{
			Name:  "disable-content-adding",
			Usage: "disallow new content ingestion globally",
			Value: cfg.ContentConfig.DisableGlobalAdding,
		},
		&cli.BoolFlag{
			Name:  "disable-local-content-adding",
			Usage: "disallow new content ingestion on this node (shuttles are unaffected)",
			Value: cfg.ContentConfig.DisableLocalAdding,
		},
		&cli.StringFlag{
			Name:  "blockstore",
			Usage: "specify blockstore parameters",
			Value: cfg.NodeConfig.BlockstoreDir,
		},
		&cli.BoolFlag{
			Name:  "write-log-truncate",
			Value: cfg.NodeConfig.WriteLogTruncate,
		},
		&cli.IntFlag{
			Name:  "default-replication",
			Value: cfg.Replication,
		},
		&cli.BoolFlag{
			Name:  "lowmem",
			Usage: "TEMP: turns down certain parameters to attempt to use less memory (will be replaced by a more specific flag later)",
			Value: cfg.LowMem,
		},
		&cli.BoolFlag{
			Name:  "jaeger-tracing",
			Value: cfg.JaegerConfig.EnableTracing,
		},
		&cli.StringFlag{
			Name:  "jaeger-provider-url",
			Value: cfg.JaegerConfig.ProviderUrl,
		},
		&cli.Float64Flag{
			Name:  "jaeger-sampler-ratio",
			Usage: "If less than 1 probabilistic metrics will be used.",
			Value: cfg.JaegerConfig.SamplerRatio,
		},
	}
	app.Commands = []*cli.Command{
		{
			Name:  "setup",
			Usage: "Creates an initial auth token under new user \"admin\"",
			Action: func(cctx *cli.Context) error {
				cfg.Load(cctx.String("config"))
				overrideSetOptions(app.Flags, cctx, cfg)
				db, err := setupDatabase(cfg)
				if err != nil {
					return err
				}

				quietdb := db.Session(&gorm.Session{
					Logger: logger.Discard,
				})

				username := "admin"
				passHash := ""

				if err := quietdb.First(&User{}, "username = ?", username).Error; err == nil {
					return fmt.Errorf("an admin user already exists")
				}

				newUser := &User{
					UUID:     uuid.New().String(),
					Username: username,
					PassHash: passHash,
					Perm:     100,
				}
				if err := db.Create(newUser).Error; err != nil {
					return fmt.Errorf("admin user creation failed: %w", err)
				}

				authToken := &AuthToken{
					Token:  "EST" + uuid.New().String() + "ARY",
					User:   newUser.ID,
					Expiry: time.Now().Add(time.Hour * 24 * 365),
				}
				if err := db.Create(authToken).Error; err != nil {
					return fmt.Errorf("admin token creation failed: %w", err)
				}

				fmt.Printf("Auth Token: %v\n", authToken.Token)

				return nil
			},
		}, {
			Name:  "configure",
			Usage: "Saves a configuration file to the location specified by the config parameter",
			Action: func(cctx *cli.Context) error {
				configuration := cctx.String("config")
				cfg.Load(configuration) // Assume error means no configuration file exists
				log.Info("test")
				overrideSetOptions(app.Flags, cctx, cfg)
				return cfg.Save(configuration)
			},
		},
	}
	app.Action = func(cctx *cli.Context) error {

		cfg.Load(cctx.String("config")) // Ignore error for now; eventually error out if no configuration file
		overrideSetOptions(app.Flags, cctx, cfg)

		db, err := setupDatabase(cfg)
		if err != nil {
			return err
		}

		init := Initializer{&cfg.NodeConfig, db, nil}

		nd, err := node.Setup(context.Background(), &init)
		if err != nil {
			return err
		}

		if err = view.Register(
			metrics.DefaultViews...,
		); err != nil {
			log.Fatalf("Cannot register the OpenCensus view: %v", err)
			return err
		}

		addr, err := nd.Wallet.GetDefault()
		if err != nil {
			return err
		}

		sbmgr, err := stagingbs.NewStagingBSMgr(cfg.StagingDataDir)
		if err != nil {
			return err
		}

		api, closer, err := lcli.GetGatewayAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		// setup tracing to jaeger if enabled
		if cfg.JaegerConfig.EnableTracing {
			tp, err := metrics.NewJaegerTraceProvider("estuary",
				cfg.JaegerConfig.ProviderUrl, cfg.JaegerConfig.SamplerRatio)
			if err != nil {
				return err
			}
			otel.SetTracerProvider(tp)
		}

		s := &Server{
			Node:        nd,
			Api:         api,
			StagingMgr:  sbmgr,
			tracer:      otel.Tracer("api"),
			cacher:      memo.NewCacher(),
			gwayHandler: gateway.NewGatewayHandler(nd.Blockstore),
		}

		// TODO: this is an ugly self referential hack... should fix
		pinmgr := pinner.NewPinManager(s.doPinning, nil, &pinner.PinManagerOpts{
			MaxActivePerUser: 20,
		})

		go pinmgr.Run(50)

		rhost := routed.Wrap(nd.Host, nd.FilDht)

		var opts []func(*filclient.Config)
		if cfg.LowMem {
			opts = append(opts, func(cfg *filclient.Config) {
				cfg.GraphsyncOpts = []gsimpl.Option{
					gsimpl.MaxInProgressIncomingRequests(100),
					gsimpl.MaxInProgressOutgoingRequests(100),
					gsimpl.MaxMemoryResponder(4 << 30),
					gsimpl.MaxMemoryPerPeerResponder(16 << 20),
					gsimpl.MaxInProgressIncomingRequestsPerPeer(10),
					gsimpl.MessageSendRetries(2),
					gsimpl.SendMessageTimeout(2 * time.Minute),
				}
			})
		}

		fc, err := filclient.NewClient(rhost, api, nd.Wallet, addr, nd.Blockstore, nd.Datastore, cfg.DataDir)
		if err != nil {
			return err
		}

		s.FilClient = fc

		for _, a := range nd.Host.Addrs() {
			fmt.Printf("%s/p2p/%s\n", a, nd.Host.ID())
		}

		go func() {
			for _, ai := range node.BootstrapPeers {
				if err := nd.Host.Connect(context.TODO(), ai); err != nil {
					fmt.Println("failed to connect to bootstrapper: ", err)
					continue
				}
			}

			if err := nd.Dht.Bootstrap(context.TODO()); err != nil {
				fmt.Println("dht bootstrapping failed: ", err)
			}
		}()

		s.DB = db

		cm, err := NewContentManager(db, api, fc, init.trackingBstore, s.Node.NotifBlockstore, nd.Provider, pinmgr, nd, cfg.Hostname)
		if err != nil {
			return err
		}

		fc.SetPieceCommFunc(cm.getPieceCommitment)

		cm.FailDealOnTransferFailure = cfg.DealConfig.FailOnTransferFailure

		cm.isDealMakingDisabled = cfg.DealConfig.Disable
		cm.contentAddingDisabled = cfg.ContentConfig.DisableGlobalAdding
		cm.localContentAddingDisabled = cfg.ContentConfig.DisableLocalAdding

		cm.tracer = otel.Tracer("replicator")

		if cctx.Bool("enable-auto-retrive") {
			init.trackingBstore.SetCidReqFunc(cm.RefreshContentForCid)
		}

		if !cfg.DisableFilecoinStorage {
			go cm.ContentWatcher()
		}

		s.CM = cm

		if !cm.contentAddingDisabled {
			go func() {
				// wait for shuttles to reconnect
				// This is a bit of a hack, and theres probably a better way to
				// solve this. but its good enough for now
				time.Sleep(time.Second * 10)

				if err := cm.refreshPinQueue(); err != nil {
					log.Errorf("failed to refresh pin queue: %s", err)
				}
			}()
		}

		// start autoretrieve index updater task every INDEX_UPDATE_INTERVAL minutes

		updateInterval, ok := os.LookupEnv("INDEX_UPDATE_INTERVAL")
		if !ok {
			updateInterval = "720"
		}
		intervalMinutes, err := strconv.Atoi(updateInterval)
		if err != nil {
			return err
		}

		s.Node.ArEngine, err = autoretrieve.NewAutoretrieveEngine(context.Background(), time.Duration(intervalMinutes)*time.Minute, s.DB, s.Node.Host)
		if err != nil {
			return err
		}

		go s.Node.ArEngine.Run()
		defer s.Node.ArEngine.Shutdown()

		go func() {
			time.Sleep(time.Second * 10)

			if err := s.RestartAllTransfersForLocation(context.TODO(), "local"); err != nil {
				log.Errorf("failed to restart transfers: %s", err)
			}
		}()

		return s.ServeAPI(cfg.ApiListen, cfg.LoggingConfig.ApiEndpointLogging, cfg.LightstepToken, filepath.Join(cfg.DataDir, "cache"))
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func setupDatabase(cfg *config.Estuary) (*gorm.DB, error) {

	/* TODO: change this default
	ddir := cctx.String("datadir")
	if dbval == defaultDatabaseValue && ddir != "." {
		dbval = "sqlite=" + filepath.Join(ddir, "estuary.db")
	}
	*/

	db, err := util.SetupDatabase(cfg.DatabaseConnString)
	if err != nil {
		return nil, err
	}

	db.AutoMigrate(&util.Content{})
	db.AutoMigrate(&util.Object{})
	db.AutoMigrate(&util.ObjRef{})
	db.AutoMigrate(&Collection{})
	db.AutoMigrate(&CollectionRef{})

	db.AutoMigrate(&contentDeal{})
	db.AutoMigrate(&dfeRecord{})
	db.AutoMigrate(&PieceCommRecord{})
	db.AutoMigrate(&proposalRecord{})
	db.AutoMigrate(&util.RetrievalFailureRecord{})
	db.AutoMigrate(&retrievalSuccessRecord{})

	db.AutoMigrate(&minerStorageAsk{})
	db.AutoMigrate(&storageMiner{})

	db.AutoMigrate(&User{})
	db.AutoMigrate(&AuthToken{})
	db.AutoMigrate(&InviteCode{})

	db.AutoMigrate(&Shuttle{})

	db.AutoMigrate(&autoretrieve.Autoretrieve{})

	// 'manually' add unique composite index on collection fields because gorms syntax for it is tricky
	if err := db.Exec("create unique index if not exists collection_refs_paths on collection_refs (path,collection)").Error; err != nil {
		return nil, fmt.Errorf("failed to create collection paths index: %w", err)
	}

	var count int64
	if err := db.Model(&storageMiner{}).Count(&count).Error; err != nil {
		return nil, err
	}

	if count == 0 {
		fmt.Println("adding default miner list to database...")
		for _, m := range build.DefaultMiners {
			db.Create(&storageMiner{Address: util.DbAddr{m}})
		}

	}
	return db, nil
}

type Server struct {
	tracer     trace.Tracer
	Node       *node.Node
	DB         *gorm.DB
	FilClient  *filclient.FilClient
	Api        api.Gateway
	CM         *ContentManager
	StagingMgr *stagingbs.StagingBSMgr

	gwayHandler *gateway.GatewayHandler

	cacher *memo.Cacher
}

func (s *Server) GarbageCollect(ctx context.Context) error {
	// since we're reference counting all the content, garbage collection becomes easy
	// its even easier if we don't care that its 'perfect'

	// We can probably even just remove stuff when its references are removed from the database
	keych, err := s.Node.Blockstore.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	for c := range keych {
		keep, err := s.trackingObject(c)
		if err != nil {
			return err
		}

		if !keep {
			// can batch these deletes and execute them at the datastore layer for more perfs
			if err := s.Node.Blockstore.DeleteBlock(ctx, c); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Server) trackingObject(c cid.Cid) (bool, error) {
	var count int64
	if err := s.DB.Model(&util.Object{}).Where("cid = ?", c.Bytes()).Count(&count).Error; err != nil {
		if xerrors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}

	return count > 0, nil
}

func jsondump(o interface{}) {
	data, _ := json.MarshalIndent(o, "", "  ")
	fmt.Println(string(data))
}

func (s *Server) RestartAllTransfersForLocation(ctx context.Context, loc string) error {
	var deals []contentDeal
	if err := s.DB.Model(contentDeal{}).
		Joins("left join contents on contents.id = content_deals.content").
		Where("not content_deals.failed and content_deals.deal_id = 0 and content_deals.dt_chan != '' and location = ?", loc).
		Scan(&deals).Error; err != nil {
		return err
	}

	for _, d := range deals {
		chid, err := d.ChannelID()
		if err != nil {
			// Only legacy (push) transfers need to be restarted by Estuary.
			// Newer (pull) transfers are restarted by the Storage Provider.
			// So if it's not a legacy channel ID, ignore it.
			continue
		}

		if err := s.CM.RestartTransfer(ctx, loc, chid); err != nil {
			log.Errorf("failed to restart transfer: %s", err)
			continue
		}
	}

	return nil
}

func (cm *ContentManager) RestartTransfer(ctx context.Context, loc string, chanid datatransfer.ChannelID) error {
	if loc == "local" {
		st, err := cm.FilClient.TransferStatus(ctx, &chanid)
		if err != nil {
			return err
		}

		if util.TransferTerminated(st) {
			return fmt.Errorf("deal in database as being in progress, but data transfer is terminated: %d", st.Status)
		}

		return cm.FilClient.RestartTransfer(ctx, &chanid)
	}

	return cm.sendRestartTransferCmd(ctx, loc, chanid)
}

func (cm *ContentManager) sendRestartTransferCmd(ctx context.Context, loc string, chanid datatransfer.ChannelID) error {
	return cm.sendShuttleCommand(ctx, loc, &drpc.Command{
		Op: drpc.CMD_RestartTransfer,
		Params: drpc.CmdParams{
			RestartTransfer: &drpc.RestartTransfer{
				ChanID: chanid,
			},
		},
	})
}
