package server

import (
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/tendermint/tendermint/abci/server"

	tcmd "github.com/tendermint/tendermint/cmd/tendermint/commands"
	cmn "github.com/tendermint/tendermint/libs/common"
	"github.com/tendermint/tendermint/node"
	"github.com/tendermint/tendermint/p2p"
	pvm "github.com/tendermint/tendermint/privval"
	"github.com/tendermint/tendermint/proxy"
)

// Tendermint full-node start flags
const (
	flagWithTendermint  = "with-tendermint"
	flagAddress         = "address"
	flagTraceStore      = "trace-store"
	flagPruning         = "pruning"
	flagCPUProfile      = "cpu-profile"
	FlagMinGasPrices    = "minimum-gas-prices"
	FlagInterBlockCache = "inter-block-cache"
)

// StartCmd runs the service passed in, either stand-alone or in-process with
// Tendermint.
func StartCmd(ctx *Context, appCreator AppCreator) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Run the full node",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !viper.GetBool(flagWithTendermint) {
				ctx.Logger.Info("starting ABCI without Tendermint")
				return startStandAlone(ctx, appCreator)
			}

			ctx.Logger.Info("starting ABCI with Tendermint")

			_, err := startInProcess(ctx, appCreator)
			return err
		},
	}

	// core flags for the ABCI application
	cmd.Flags().Bool(flagWithTendermint, true, "Run abci app embedded in-process with tendermint")
	cmd.Flags().String(flagAddress, "tcp://0.0.0.0:26658", "Listen address")
	cmd.Flags().String(flagTraceStore, "", "Enable KVStore tracing to an output file")
	cmd.Flags().String(flagPruning, "syncable", "Pruning strategy: syncable, nothing, everything")
	cmd.Flags().String(
		FlagMinGasPrices, "",
		"Minimum gas prices to accept for transactions; Any fee in a tx must meet this minimum (e.g. 0.01photino;0.0001stake)",
	)
	cmd.Flags().String(flagCPUProfile, "", "Enable CPU profiling and write to the file")
	cmd.Flags().Bool(FlagInterBlockCache, false, "Enable inter-block caching")

	// add support for all Tendermint-specific command line options
	tcmd.AddNodeFlags(cmd)
	return cmd
}

func startStandAlone(ctx *Context, appCreator AppCreator) error {
	addr := viper.GetString(flagAddress)
	home := viper.GetString("home")
	traceWriterFile := viper.GetString(flagTraceStore)

	db, err := openDB(home)
	if err != nil {
		return err
	}
	traceWriter, err := openTraceWriter(traceWriterFile)
	if err != nil {
		return err
	}

	app := appCreator(ctx.Logger, db, traceWriter)

	svr, err := server.NewServer(addr, "socket", app)
	if err != nil {
		return fmt.Errorf("error creating listener: %v", err)
	}

	svr.SetLogger(ctx.Logger.With("module", "abci-server"))

	err = svr.Start()
	if err != nil {
		cmn.Exit(err.Error())
	}

	// wait forever
	cmn.TrapSignal(ctx.Logger, func() {
		// cleanup
		err = svr.Stop()
		if err != nil {
			cmn.Exit(err.Error())
		}
	})
	return nil
}

func startInProcess(ctx *Context, appCreator AppCreator) (*node.Node, error) {
	cfg := ctx.Config
	home := cfg.RootDir
	traceWriterFile := viper.GetString(flagTraceStore)

	db, err := openDB(home)
	if err != nil {
		return nil, err
	}
	traceWriter, err := openTraceWriter(traceWriterFile)
	if err != nil {
		return nil, err
	}

	app := appCreator(ctx.Logger, db, traceWriter)

	nodeKey, err := p2p.LoadOrGenNodeKey(cfg.NodeKeyFile())
	if err != nil {
		return nil, err
	}

	UpgradeOldPrivValFile(cfg)
	// create & start tendermint node
	tmNode, err := node.NewNode(
		cfg,
		pvm.LoadOrGenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile()),
		nodeKey,
		proxy.NewLocalClientCreator(app),
		node.DefaultGenesisDocProviderFunc(cfg),
		node.DefaultDBProvider,
		node.DefaultMetricsProvider(cfg.Instrumentation),
		ctx.Logger.With("module", "node"),
	)
	if err != nil {
		return nil, err
	}

	err = tmNode.Start()
	if err != nil {
		return nil, err
	}

	cpuProfile := viper.GetString(flagCPUProfile)
	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			return nil, err
		}

		defer f.Close()

		ctx.Logger.Info("starting CPU profiler", "profile", cpuProfile)
		if err := pprof.StartCPUProfile(f); err != nil {
			return nil, err
		}
	}

	TrapSignal(func() {
		ctx.Logger.Info("stopping CPU profiler", "profile", cpuProfile)
		pprof.StopCPUProfile()

		if tmNode.IsRunning() {
			_ = tmNode.Stop()
		}

		ctx.Logger.Info("exiting...")
	})

	// run forever (the node will not be returned)
	select {}
}
