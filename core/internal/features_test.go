package internal_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smartcontractkit/chainlink/core/assets"
	"github.com/smartcontractkit/chainlink/core/auth"
	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/internal/mocks"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/store/orm"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/smartcontractkit/chainlink/core/web"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var oneETH = assets.Eth(*big.NewInt(1000000000000000000))

func TestIntegration_Scheduler(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	app.Start()

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/scheduler_job.json")

	cltest.WaitForRunsAtLeast(t, j, app.Store, 1)

	initr := j.Initiators[0]
	assert.Equal(t, models.InitiatorCron, initr.Type)
	assert.Equal(t, "CRON_TZ=UTC * * * * * *", string(initr.Schedule), "Wrong cron schedule saved")
}

func TestIntegration_HttpRequestWithHeaders(t *testing.T) {
	config, cfgCleanup := cltest.NewConfig(t)
	defer cfgCleanup()

	gethClient := new(mocks.GethClient)
	rpcClient := new(mocks.RPCClient)
	sub := new(mocks.Subscription)
	chchNewHeads := make(chan chan<- *models.Head, 1)

	app, appCleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		eth.NewClientWith(rpcClient, gethClient),
	)
	defer appCleanup()

	tickerHeaders := http.Header{
		"Key1": []string{"value"},
		"Key2": []string{"value", "value"},
	}
	tickerResponse := `{"high": "10744.00", "last": "10583.75", "timestamp": "1512156162", "bid": "10555.13", "vwap": "10097.98", "volume": "17861.33960013", "low": "9370.11", "ask": "10583.00", "open": "9927.29"}`
	mockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "GET", tickerResponse,
		func(header http.Header, _ string) {
			for key, values := range tickerHeaders {
				assert.Equal(t, values, header[key])
			}
		})
	defer assertCalled()

	confirmed := int64(23456)
	safe := confirmed + int64(config.MinRequiredOutgoingConfirmations())
	inLongestChain := safe - int64(config.GasUpdaterBlockDelay())

	rpcClient.On("EthSubscribe", mock.Anything, mock.Anything, "newHeads").
		Run(func(args mock.Arguments) { chchNewHeads <- args.Get(1).(chan<- *models.Head) }).
		Return(sub, nil)
	rpcClient.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(inLongestChain)
		}).
		Return(nil)

	gethClient.On("ChainID", mock.Anything).Return(config.ChainID(), nil)
	gethClient.On("BalanceAt", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(oneETH.ToInt(), nil)
	gethClient.On("BlockByNumber", mock.Anything, big.NewInt(inLongestChain)).Return(cltest.BlockWithTransactions(), nil)

	gethClient.On("SendTransaction", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			tx, ok := args.Get(1).(*types.Transaction)
			require.True(t, ok)
			gethClient.On("TransactionReceipt", mock.Anything, mock.Anything).
				Return(&types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(confirmed)}, nil)
		}).
		Return(nil).Once()

	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return(nil).Maybe()

	assert.NoError(t, app.StartAndConnect())

	newHeads := <-chchNewHeads

	j := cltest.CreateHelloWorldJobViaWeb(t, app, mockServer.URL)
	jr := cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, cltest.CreateJobRunViaWeb(t, app, j))
	cltest.WaitForEthTxAttemptCount(t, app.Store, 1)

	// Do the thing
	newHeads <- cltest.Head(safe)

	cltest.WaitForJobRunToComplete(t, app.Store, jr)
}

func TestIntegration_FeeBump_LegacyTXM(t *testing.T) {
	tickerResponse := `{"high": "10744.00", "last": "10583.75", "timestamp": "1512156162", "bid": "10555.13", "vwap": "10097.98", "volume": "17861.33960013", "low": "9370.11", "ask": "10583.00", "open": "9927.29"}`
	mockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "GET", tickerResponse)
	defer assertCalled()

	config, cleanup := cltest.NewConfig(t)
	defer cleanup()
	config.Set("ENABLE_BULLETPROOF_TX_MANAGER", false) // TODO - remove with test
	// Must use hardcoded key here since the hash has to match attempt1Hash
	app, cleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		cltest.LenientEthMock,
		cltest.EthMockRegisterGetBalance,
		cltest.EthMockRegisterGetBlockByNumber,
	)
	defer cleanup()

	// Put some distance between these two values so we can explore more of the state space
	config.Set("ETH_GAS_BUMP_THRESHOLD", 10)
	config.Set("MIN_OUTGOING_CONFIRMATIONS", 20)

	attempt1Hash := common.HexToHash("0xb7862c896a6ba2711bccc0410184e46d793ea83b3e05470f1d359ea276d16bb5")
	attempt2Hash := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002")
	attempt3Hash := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000003")

	unconfirmedReceipt := (*types.Receipt)(nil)

	// Enumerate the different block heights at which various state changes
	// happen for the transaction attempts created during this test
	firstTxSentAt := int64(23456)
	firstTxGasBumpAt := firstTxSentAt + int64(config.EthGasBumpThreshold())
	firstTxRemainsUnconfirmedAt := firstTxGasBumpAt - 1

	secondTxSentAt := firstTxGasBumpAt
	secondTxGasBumpAt := secondTxSentAt + int64(config.EthGasBumpThreshold())
	secondTxRemainsUnconfirmedAt := secondTxGasBumpAt - 1

	thirdTxSentAt := secondTxGasBumpAt
	thirdTxConfirmedAt := thirdTxSentAt + 1
	thirdTxConfirmedReceipt := &types.Receipt{
		TxHash:      attempt1Hash,
		BlockNumber: big.NewInt(thirdTxConfirmedAt),
	}
	thirdTxSafeAt := thirdTxSentAt + int64(config.MinRequiredOutgoingConfirmations())

	newHeads := make(chan *models.Head)
	eth := app.EthMock
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("newHeads", newHeads)
		eth.Register("eth_chainId", config.ChainID())
		eth.Register("eth_getTransactionCount", `0x100`) // TxManager.ActivateAccount()
	})
	require.NoError(t, app.Store.ORM.IdempotentInsertHead(*cltest.Head(firstTxSentAt)))
	assert.NoError(t, app.Start())
	eth.EventuallyAllCalled(t)

	// This first run of the EthTx adapter creates an initial transaction which
	// starts unconfirmed
	eth.Context("ethTx.Perform()#1", func(eth *cltest.EthMock) {
		eth.Register("eth_sendRawTransaction", attempt1Hash)
		eth.Register("eth_getTransactionReceipt", unconfirmedReceipt)
	})
	j := cltest.CreateHelloWorldJobViaWeb(t, app, mockServer.URL)
	jr := cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, cltest.CreateJobRunViaWeb(t, app, j))
	eth.EventuallyAllCalled(t)
	cltest.WaitForTxAttemptCount(t, app.Store, 1)

	// At the next head, the transaction is still unconfirmed, but no thresholds
	// have been met so we just wait...
	newHeads <- cltest.Head(firstTxRemainsUnconfirmedAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)

	// At the next head, the transaction remains unconfirmed but the gas bump
	// threshold has been met, so a new transaction is made with a higher amount
	// of gas ("bumped gas")
	eth.Context("ethTx.Perform()#3", func(eth *cltest.EthMock) {
		eth.Register("eth_sendRawTransaction", attempt2Hash)
		eth.Register("eth_getTransactionReceipt", unconfirmedReceipt)
	})
	newHeads <- cltest.Head(firstTxGasBumpAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)
	cltest.WaitForTxAttemptCount(t, app.Store, 2)

	// Another head comes in and both transactions are still unconfirmed, more
	// waiting...
	newHeads <- cltest.Head(secondTxRemainsUnconfirmedAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)

	// Now the second transaction attempt meets the gas bump threshold, so a
	// final transaction attempt shoud be made
	eth.Context("ethTx.Perform()#5", func(eth *cltest.EthMock) {
		eth.Register("eth_getTransactionReceipt", unconfirmedReceipt)
		eth.Register("eth_sendRawTransaction", attempt3Hash)
	})
	newHeads <- cltest.Head(secondTxGasBumpAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)
	cltest.WaitForTxAttemptCount(t, app.Store, 3)

	// This third attempt has enough gas and gets confirmed, but has not yet
	// received sufficient confirmations, so we wait again...
	newHeads <- cltest.Head(thirdTxConfirmedAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)

	// Finally the third attempt gets to a minimum number of safe confirmations,
	eth.Context("ethTx.Perform()#7", func(eth *cltest.EthMock) {
		eth.RegisterOptional("eth_getTransactionReceipt", &types.Receipt{})
		eth.RegisterOptional("eth_getTransactionReceipt", thirdTxConfirmedReceipt)
		eth.RegisterOptional("eth_sendRawTransaction", attempt3Hash)
	})
	newHeads <- cltest.Head(thirdTxSafeAt)
	eth.EventuallyAllCalled(t)
	jr = cltest.WaitForJobRunToComplete(t, app.Store, jr)

	require.Len(t, jr.TaskRuns, 4)
	value := cltest.MustResultString(t, jr.TaskRuns[0].Result)
	assert.Equal(t, tickerResponse, value)
	value = cltest.MustResultString(t, jr.TaskRuns[1].Result)
	assert.Equal(t, "10583.75", value)
	value = cltest.MustResultString(t, jr.TaskRuns[3].Result)
	assert.Equal(t, attempt1Hash.String(), value)
	value = cltest.MustResultString(t, jr.Result)
	assert.Equal(t, attempt1Hash.String(), value)
}

func TestIntegration_FeeBump_RunLog(t *testing.T) {
	tickerResponse := `{"RAW":{"ETH":{"USD":{"TYPE":"5","MARKET":"CCCAGG","FROMSYMBOL":"ETH","TOSYMBOL":"USD","FLAGS":"2052","PRICE":383.64,"LASTUPDATE":1604436392,"MEDIAN":383.66,"LASTVOLUME":0.0792252,"LASTVOLUMETO":30.378110688,"LASTTRADEID":"94484630","VOLUMEDAY":117102.19653678121,"VOLUMEDAYTO":44476030.58997059,"VOLUME24HOUR":278503.64621400996,"VOLUME24HOURTO":105749370.4340889,"OPENDAY":383.61,"HIGHDAY":385.58,"LOWDAY":370.79,"OPEN24HOUR":388.14,"HIGH24HOUR":388.29,"LOW24HOUR":372.39,"LASTMARKET":"BTCAlpha","VOLUMEHOUR":3651.825436420002,"VOLUMEHOURTO":1400820.631646926,"OPENHOUR":383.53,"HIGHHOUR":384.04,"LOWHOUR":382.95,"TOPTIERVOLUME24HOUR":277893.13967487996,"TOPTIERVOLUME24HOURTO":105517085.04526761,"CHANGE24HOUR":-4.5,"CHANGEPCT24HOUR":-1.159375483073118,"CHANGEDAY":0.029999999999972715,"CHANGEPCTDAY":0.007820442637046144,"CHANGEHOUR":0.11000000000001364,"CHANGEPCTHOUR":0.02868093760592748,"CONVERSIONTYPE":"direct","CONVERSIONSYMBOL":"","SUPPLY":112517755.749,"MKTCAP":43166311815.54636,"TOTALVOLUME24H":3840997.0686040893,"TOTALVOLUME24HTO":1472464346.9998188,"TOTALTOPTIERVOLUME24H":3712060.528468081,"TOTALTOPTIERVOLUME24HTO":1423001062.081891,"IMAGEURL":"/media/20646/eth_logo.png"}}},"DISPLAY":{"ETH":{"USD":{"FROMSYMBOL":"Ξ","TOSYMBOL":"$","MARKET":"CryptoCompare Index","PRICE":"$ 383.64","LASTUPDATE":"Just now","LASTVOLUME":"Ξ 0.07923","LASTVOLUMETO":"$ 30.38","LASTTRADEID":"94484630","VOLUMEDAY":"Ξ 117,102.2","VOLUMEDAYTO":"$ 44,476,030.6","VOLUME24HOUR":"Ξ 278,503.6","VOLUME24HOURTO":"$ 105,749,370.4","OPENDAY":"$ 383.61","HIGHDAY":"$ 385.58","LOWDAY":"$ 370.79","OPEN24HOUR":"$ 388.14","HIGH24HOUR":"$ 388.29","LOW24HOUR":"$ 372.39","LASTMARKET":"BTCAlpha","VOLUMEHOUR":"Ξ 3,651.83","VOLUMEHOURTO":"$ 1,400,820.6","OPENHOUR":"$ 383.53","HIGHHOUR":"$ 384.04","LOWHOUR":"$ 382.95","TOPTIERVOLUME24HOUR":"Ξ 277,893.1","TOPTIERVOLUME24HOURTO":"$ 105,517,085.0","CHANGE24HOUR":"$ -4.50","CHANGEPCT24HOUR":"-1.16","CHANGEDAY":"$ 0.030","CHANGEPCTDAY":"0.01","CHANGEHOUR":"$ 0.11","CHANGEPCTHOUR":"0.03","CONVERSIONTYPE":"direct","CONVERSIONSYMBOL":"","SUPPLY":"Ξ 112,517,755.7","MKTCAP":"$ 43.17 B","TOTALVOLUME24H":"Ξ 3.84 M","TOTALVOLUME24HTO":"$ 1.47 B","TOTALTOPTIERVOLUME24H":"Ξ 3.71 M","TOTALTOPTIERVOLUME24HTO":"$ 1.42 B","IMAGEURL":"/media/20646/eth_logo.png"}}}}`
	mockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "GET", tickerResponse)
	defer assertCalled()

	config, cleanup := cltest.NewConfig(t)
	defer cleanup()

	// Must use hardcoded key here since the hash has to match attempt1Hash
	app, cleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		cltest.LenientEthMock,
		cltest.EthMockRegisterGetBalance,
		cltest.EthMockRegisterGetBlockByNumber,
	)
	defer cleanup()

	config.Set("ENABLE_BULLETPROOF_TX_MANAGER", false)
	config.Set("GAS_UPDATER_ENABLED", false)

	// Put some distance between these two values so we can explore more of the state space
	config.Set("ETH_GAS_BUMP_THRESHOLD", 10)
	config.Set("MIN_OUTGOING_CONFIRMATIONS", 20)
	config.Set("MIN_INCOMING_CONFIRMATIONS", 3)

	newHeads := make(chan *models.Head)
	eth := app.EthMock
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("newHeads", newHeads)
		eth.Register("eth_chainId", config.ChainID())
	})
	assert.NoError(t, app.Start())
	eth.EventuallyAllCalled(t)

	logs := make(chan types.Log, 1)
	eth.Context("Creating run log job, subscribes to logs", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("logs", logs)
	})
	j := cltest.FixtureCreateJobViaWeb(t, app, "testdata/hello_world_job_run_log.json")
	initr := j.Initiators[0]
	assert.Equal(t, models.InitiatorRunLog, initr.Type)
	eth.EventuallyAllCalled(t)

	// Wake job up, it will be paused pending confirmations
	input := fmt.Sprintf(`{"url": "%s", "path": "RAW.ETH.USD.VOLUME24HOUR", "times": "1000000000000000000"}`, mockServer.URL)
	runlog := cltest.NewRunLog(t, j.ID, cltest.NewAddress(), cltest.NewAddress(), int(0), input)
	logs <- runlog
	cltest.WaitForRuns(t, j, app.Store, 1)
	eth.EventuallyAllCalled(t)

	eth.Context("Run is triggered by new head", func(eth *cltest.EthMock) {
		eth.Register("eth_getTransactionReceipt", &types.Receipt{TxHash: runlog.TxHash, BlockHash: runlog.BlockHash})
	})
	newHeads <- cltest.Head(3)
	eth.EventuallyAllCalled(t)

	// Make sure job completed
	runs, err := app.Store.JobRunsFor(j.ID)
	assert.NoError(t, err)
	jr := runs[0]
	cltest.WaitForJobRunStatus(t, app.Store, jr, models.RunStatusPendingConnection)
}

func TestIntegration_RunAt(t *testing.T) {
	t.Parallel()
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	app.InstantClock()

	require.NoError(t, app.Start())
	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/run_at_job.json")

	initr := j.Initiators[0]
	assert.Equal(t, models.InitiatorRunAt, initr.Type)
	assert.Equal(t, "2018-01-08T18:12:01Z", utils.ISO8601UTC(initr.Time.Time))

	jrs := cltest.WaitForRuns(t, j, app.Store, 1)
	cltest.WaitForJobRunToComplete(t, app.Store, jrs[0])
}

func TestIntegration_EthLog(t *testing.T) {
	t.Parallel()
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()

	eth := app.EthMock
	logs := make(chan models.Log, 1)
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("logs", logs)
		eth.Register("eth_getTransactionReceipt", &types.Receipt{})
	})
	require.NoError(t, app.StartAndConnect())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/eth_log_job.json")
	address := common.HexToAddress("0x3cCad4715152693fE3BC4460591e3D3Fbd071b42")

	initr := j.Initiators[0]
	assert.Equal(t, models.InitiatorEthLog, initr.Type)
	assert.Equal(t, address, initr.Address)

	logs <- cltest.LogFromFixture(t, "testdata/requestLog0original.json")
	jrs := cltest.WaitForRuns(t, j, app.Store, 1)
	cltest.WaitForJobRunToComplete(t, app.Store, jrs[0])
}

func TestIntegration_RunLog(t *testing.T) {
	triggeringBlockHash := cltest.NewHash()
	otherBlockHash := cltest.NewHash()

	tests := []struct {
		name             string
		logBlockHash     common.Hash
		receiptBlockHash common.Hash
		wantStatus       models.RunStatus
	}{
		{
			name:             "completed",
			logBlockHash:     triggeringBlockHash,
			receiptBlockHash: triggeringBlockHash,
			wantStatus:       models.RunStatusCompleted,
		},
		{
			name:             "ommered request",
			logBlockHash:     triggeringBlockHash,
			receiptBlockHash: otherBlockHash,
			wantStatus:       models.RunStatusErrored,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, cfgCleanup := cltest.NewConfig(t)
			defer cfgCleanup()
			config.Set("MIN_INCOMING_CONFIRMATIONS", 6)
			app, cleanup := cltest.NewApplicationWithConfig(t, config,
				cltest.LenientEthMock,
				cltest.EthMockRegisterGetBlockByNumber,
				cltest.EthMockRegisterGetBalance,
			)
			defer cleanup()

			eth := app.EthMock
			logs := make(chan types.Log, 1)
			newHeads := eth.RegisterNewHeads()
			eth.Context("app.Start()", func(eth *cltest.EthMock) {
				eth.RegisterSubscription("logs", logs)
			})
			eth.Register("eth_chainId", config.ChainID())
			require.NoError(t, app.Start())

			j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/runlog_noop_job.json")
			requiredConfs := int64(100)

			initr := j.Initiators[0]
			assert.Equal(t, models.InitiatorRunLog, initr.Type)

			creationHeight := int64(1)
			runlog := cltest.NewRunLog(t, j.ID, cltest.NewAddress(), cltest.NewAddress(), int(creationHeight), `{}`)
			runlog.BlockHash = test.logBlockHash
			logs <- runlog
			cltest.WaitForRuns(t, j, app.Store, 1)

			runs, err := app.Store.JobRunsFor(j.ID)
			assert.NoError(t, err)
			jr := runs[0]
			cltest.WaitForJobRunToPendIncomingConfirmations(t, app.Store, jr)
			require.Len(t, jr.TaskRuns, 1)
			assert.False(t, jr.TaskRuns[0].ObservedIncomingConfirmations.Valid)

			blockIncrease := int64(app.Store.Config.MinIncomingConfirmations())
			minGlobalHeight := creationHeight + blockIncrease
			newHeads <- cltest.Head(minGlobalHeight)
			<-time.After(time.Second)
			jr = cltest.JobRunStaysPendingIncomingConfirmations(t, app.Store, jr)
			assert.Equal(t, int64(creationHeight+blockIncrease), int64(jr.TaskRuns[0].ObservedIncomingConfirmations.Uint32))

			safeNumber := creationHeight + requiredConfs
			newHeads <- cltest.Head(safeNumber)
			confirmedReceipt := &types.Receipt{
				TxHash:      runlog.TxHash,
				BlockHash:   test.receiptBlockHash,
				BlockNumber: big.NewInt(creationHeight),
			}
			eth.Context("validateOnMainChain", func(ethMock *cltest.EthMock) {
				eth.Register("eth_getTransactionReceipt", confirmedReceipt)
			})

			jr = cltest.WaitForJobRunStatus(t, app.Store, jr, test.wantStatus)
			assert.True(t, jr.FinishedAt.Valid)
			assert.Equal(t, int64(requiredConfs), int64(jr.TaskRuns[0].ObservedIncomingConfirmations.Uint32))
			assert.True(t, eth.AllCalled(), eth.Remaining())
		})
	}
}

func TestIntegration_StartAt(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	eth := app.EthMock
	eth.Register("eth_chainId", app.Store.Config.ChainID())
	require.NoError(t, app.Start())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/start_at_job.json")
	startAt := cltest.ParseISO8601(t, "1970-01-01T00:00:00.000Z")
	assert.Equal(t, startAt, j.StartAt.Time)

	jr := cltest.CreateJobRunViaWeb(t, app, j)
	cltest.WaitForJobRunToComplete(t, app.Store, jr)
}

func TestIntegration_ExternalAdapter_RunLogInitiated(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterGetBlockByNumber,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()

	eth := app.EthMock
	eth.Register("eth_chainId", app.Store.Config.ChainID())
	logs := make(chan models.Log, 1)
	newHeads := make(chan *models.Head, 10)
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("logs", logs)
		eth.RegisterSubscription("newHeads", newHeads)
	})
	require.NoError(t, app.Start())

	eaValue := "87698118359"
	eaExtra := "other values to be used by external adapters"
	eaResponse := fmt.Sprintf(`{"data":{"result": "%v", "extra": "%v"}}`, eaValue, eaExtra)
	mockServer, ensureRequest := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", eaResponse)
	defer ensureRequest()

	bridgeJSON := fmt.Sprintf(`{"name":"randomNumber","url":"%v","confirmations":10}`, mockServer.URL)
	cltest.CreateBridgeTypeViaWeb(t, app, bridgeJSON)
	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/log_initiated_bridge_type_job.json")

	logBlockNumber := 1
	runlog := cltest.NewRunLog(t, j.ID, cltest.NewAddress(), cltest.NewAddress(), logBlockNumber, `{}`)
	logs <- runlog
	jr := cltest.WaitForRuns(t, j, app.Store, 1)[0]
	cltest.WaitForJobRunToPendIncomingConfirmations(t, app.Store, jr)

	newHeads <- cltest.Head(logBlockNumber + 8)
	cltest.WaitForJobRunToPendIncomingConfirmations(t, app.Store, jr)

	confirmedReceipt := &types.Receipt{
		TxHash:      runlog.TxHash,
		BlockHash:   runlog.BlockHash,
		BlockNumber: big.NewInt(int64(logBlockNumber)),
	}
	eth.Context("validateOnMainChain", func(ethMock *cltest.EthMock) {
		eth.Register("eth_getTransactionReceipt", confirmedReceipt)
	})

	newHeads <- cltest.Head(logBlockNumber + 9)
	jr = cltest.WaitForJobRunToComplete(t, app.Store, jr)

	tr := jr.TaskRuns[0]
	assert.Equal(t, "randomnumber", tr.TaskSpec.Type.String())
	value := cltest.MustResultString(t, tr.Result)
	assert.Equal(t, eaValue, value)
	res := tr.Result.Data.Get("extra")
	assert.Equal(t, eaExtra, res.String())

	assert.True(t, eth.AllCalled(), eth.Remaining())
}

// This test ensures that the response body of an external adapter are supplied
// as params to the successive task
func TestIntegration_ExternalAdapter_Copy(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	bridgeURL := cltest.WebURL(t, "https://test.chain.link/always")
	app.Store.Config.Set("BRIDGE_RESPONSE_URL", bridgeURL)
	require.NoError(t, app.Start())

	eaPrice := "1234"
	eaQuote := "USD"
	eaResponse := fmt.Sprintf(`{"data":{"price": "%v", "quote": "%v"}}`, eaPrice, eaQuote)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/", r.URL.Path)

		b, err := ioutil.ReadAll(r.Body)
		require.NoError(t, err)
		body := cltest.JSONFromBytes(t, b)
		data := body.Get("data")
		require.True(t, data.Exists())
		bodyParam := data.Get("bodyParam")
		require.True(t, bodyParam.Exists())
		require.Equal(t, true, bodyParam.Bool())

		url := body.Get("responseURL")
		require.Contains(t, url.String(), "https://test.chain.link/always/v2/runs")

		w.WriteHeader(http.StatusOK)
		io.WriteString(w, eaResponse)
	}))
	defer ts.Close()

	bridgeJSON := fmt.Sprintf(`{"name":"assetPrice","url":"%v"}`, ts.URL)
	cltest.CreateBridgeTypeViaWeb(t, app, bridgeJSON)
	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/bridge_type_copy_job.json")
	jr := cltest.WaitForJobRunToComplete(t, app.Store, cltest.CreateJobRunViaWeb(t, app, j, `{"copyPath": ["price"]}`))

	tr := jr.TaskRuns[0]
	assert.Equal(t, "assetprice", tr.TaskSpec.Type.String())
	tr = jr.TaskRuns[1]
	assert.Equal(t, "copy", tr.TaskSpec.Type.String())
	value := cltest.MustResultString(t, tr.Result)
	assert.Equal(t, eaPrice, value)
}

// This test ensures that an bridge adapter task is resumed from pending after
// sending out a request to an external adapter and waiting to receive a
// request back
func TestIntegration_ExternalAdapter_Pending(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	require.NoError(t, app.Start())

	bta := &models.BridgeTypeAuthentication{}
	var j models.JobSpec
	mockServer, cleanup := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", `{"pending":true}`,
		func(h http.Header, b string) {
			body := cltest.JSONFromString(t, b)

			jrs := cltest.WaitForRuns(t, j, app.Store, 1)
			jr := jrs[0]
			id := body.Get("id")
			assert.True(t, id.Exists())
			assert.Equal(t, jr.ID.String(), id.String())

			data := body.Get("data")
			assert.True(t, data.Exists())
			assert.Equal(t, data.Type, gjson.JSON)

			token := utils.StripBearer(h.Get("Authorization"))
			assert.Equal(t, bta.OutgoingToken, token)
		})
	defer cleanup()

	bridgeJSON := fmt.Sprintf(`{"name":"randomNumber","url":"%v"}`, mockServer.URL)
	bta = cltest.CreateBridgeTypeViaWeb(t, app, bridgeJSON)
	j = cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/random_number_bridge_type_job.json")
	jr := cltest.CreateJobRunViaWeb(t, app, j)
	jr = cltest.WaitForJobRunToPendBridge(t, app.Store, jr)

	tr := jr.TaskRuns[0]
	assert.Equal(t, models.RunStatusPendingBridge, tr.Status)
	assert.Equal(t, gjson.Null, tr.Result.Data.Get("result").Type)

	jr = cltest.UpdateJobRunViaWeb(t, app, jr, bta, `{"data":{"result":"100"}}`)
	jr = cltest.WaitForJobRunToComplete(t, app.Store, jr)
	tr = jr.TaskRuns[0]
	assert.Equal(t, models.RunStatusCompleted, tr.Status)

	value := cltest.MustResultString(t, tr.Result)
	assert.Equal(t, "100", value)
}

func TestIntegration_WeiWatchers(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterGetBlockByNumber,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()

	eth := app.EthMock
	eth.RegisterNewHead(1)
	logs := make(chan models.Log, 1)
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.Register("eth_chainId", app.Config.ChainID())
		eth.RegisterSubscription("logs", logs)
		eth.Register("eth_getTransactionReceipt", &types.Receipt{})
	})

	log := cltest.LogFromFixture(t, "testdata/requestLog0original.json")
	mockServer, cleanup := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", `{"pending":true}`,
		func(_ http.Header, body string) {
			marshaledLog, err := json.Marshal(&log)
			assert.NoError(t, err)
			assert.JSONEq(t, string(marshaledLog), body)
		})
	defer cleanup()

	require.NoError(t, app.Start())

	j := cltest.NewJobWithLogInitiator()
	post := cltest.NewTask(t, "httppostwithunrestrictednetworkaccess", fmt.Sprintf(`{"url":"%v"}`, mockServer.URL))
	tasks := []models.TaskSpec{post}
	j.Tasks = tasks
	j = cltest.CreateJobSpecViaWeb(t, app, j)

	logs <- log

	jobRuns := cltest.WaitForRuns(t, j, app.Store, 1)
	cltest.WaitForJobRunToComplete(t, app.Store, jobRuns[0])
}

func TestIntegration_MultiplierInt256(t *testing.T) {
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	require.NoError(t, app.Start())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/int256_job.json")
	jr := cltest.CreateJobRunViaWeb(t, app, j, `{"result":"-10221.30"}`)
	jr = cltest.WaitForJobRunToComplete(t, app.Store, jr)

	value := cltest.MustResultString(t, jr.Result)
	assert.Equal(t, "0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0674e", value)
}

func TestIntegration_MultiplierUint256(t *testing.T) {
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	require.NoError(t, app.Start())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/uint256_job.json")
	jr := cltest.CreateJobRunViaWeb(t, app, j, `{"result":"10221.30"}`)
	jr = cltest.WaitForJobRunToComplete(t, app.Store, jr)

	value := cltest.MustResultString(t, jr.Result)
	assert.Equal(t, "0x00000000000000000000000000000000000000000000000000000000000f98b2", value)
}

func TestIntegration_NonceManagement_firstRunWithExistingTxs_LegacyTXM(t *testing.T) {
	t.Parallel()

	config, cleanup := cltest.NewConfig(t)
	defer cleanup()
	config.Set("ENABLE_BULLETPROOF_TX_MANAGER", false) // TODO - remove with test
	app, cleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBlockByNumber,
	)
	defer cleanup()

	eth := app.EthMock
	newHeads := make(chan *models.Head)
	eth.Context("app.Start()", func(eth *cltest.EthMock) {
		eth.RegisterSubscription("newHeads", newHeads)
		eth.Register("eth_getTransactionCount", `0x100`) // activate account nonce
	})
	require.NoError(t, app.Store.ORM.IdempotentInsertHead(*cltest.Head(100)))
	require.NoError(t, app.StartAndConnect())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/web_initiated_eth_tx_job.json")
	hash1 := common.HexToHash("0x34c4fbd25473129a88d5a835a11a293f09941c0a198fbbb26a6b0521181ac08d")
	blockNumber := int64(100 - app.Store.Config.MinRequiredOutgoingConfirmations())

	eth.Context("ethTx.Perform()", func(eth *cltest.EthMock) {
		eth.Register("eth_getTransactionReceipt", &types.Receipt{
			TxHash:      hash1,
			BlockNumber: big.NewInt(blockNumber),
		})
		eth.Register("eth_sendRawTransaction", hash1)
	})

	jr := cltest.CreateJobRunViaWeb(t, app, j, `{"result":"0x11"}`)
	cltest.WaitForJobRunToComplete(t, app.Store, jr)

	attempt := cltest.GetLastTxAttempt(t, app.Store)
	tx, err := app.Store.FindTx(attempt.TxID)
	assert.NoError(t, err)
	assert.Equal(t, uint64(0x100), tx.Nonce)

	eth.AssertAllCalled()

	newHeads <- cltest.Head(200)
	eth.EventuallyAllCalled(t)
	hash2 := common.HexToHash("0x0881908e6aac65c32e28d280e51033170aff97926d096a19b46cc9aa163b80cc")
	hash3 := common.HexToHash("0x66c6936fd241ec3061133fa8add47fd340e8f1b29d13b3211108620a45970b04")

	eth.Context("ethTx.Perform()", func(eth *cltest.EthMock) {
		eth.Register("eth_getTransactionReceipt", &types.Receipt{
			TxHash:      hash2,
			BlockNumber: big.NewInt(blockNumber + 100),
		})
		eth.Register("eth_sendRawTransaction", hash2)
		eth.Register("eth_getTransactionReceipt", &types.Receipt{
			TxHash:      hash3,
			BlockNumber: big.NewInt(blockNumber + 100),
		})
		eth.Register("eth_sendRawTransaction", hash3)
	})

	jr = cltest.CreateJobRunViaWeb(t, app, j, `{"result":"0x11"}`)
	cltest.WaitForJobRunToComplete(t, app.Store, jr)

	attempt = cltest.GetLastTxAttempt(t, app.Store)
	tx, err = app.Store.FindTx(attempt.TxID)
	assert.NoError(t, err)
	assert.Equal(t, uint64(0x102), tx.Nonce)

	eth.AssertAllCalled()
}

func TestIntegration_SyncJobRuns(t *testing.T) {
	t.Parallel()
	wsserver, wsserverCleanup := cltest.NewEventWebSocketServer(t)
	defer wsserverCleanup()

	config, _ := cltest.NewConfig(t)
	config.Set("EXPLORER_URL", wsserver.URL.String())
	app, cleanup := cltest.NewApplicationWithConfig(t, config,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	kst := new(mocks.KeyStoreInterface)
	app.Store.KeyStore = kst
	defer cleanup()

	app.InstantClock()
	require.NoError(t, app.Start())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/run_at_job.json")

	cltest.CallbackOrTimeout(t, "stats pusher connects", func() {
		<-wsserver.Connected
	}, 5*time.Second)

	var message string
	cltest.CallbackOrTimeout(t, "stats pusher sends", func() {
		message = <-wsserver.Received
	}, 5*time.Second)

	var run models.JobRun
	err := json.Unmarshal([]byte(message), &run)
	require.NoError(t, err)
	assert.Equal(t, j.ID, run.JobSpecID)
	cltest.WaitForJobRunToComplete(t, app.Store, run)
	kst.AssertExpectations(t)
}

func TestIntegration_SleepAdapter(t *testing.T) {
	t.Parallel()

	sleepSeconds := 4
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	app.Config.Set("ENABLE_EXPERIMENTAL_ADAPTERS", "true")
	defer cleanup()
	require.NoError(t, app.Start())

	j := cltest.FixtureCreateJobViaWeb(t, app, "./testdata/sleep_job.json")

	runInput := fmt.Sprintf("{\"until\": \"%s\"}", time.Now().Local().Add(time.Second*time.Duration(sleepSeconds)))
	jr := cltest.CreateJobRunViaWeb(t, app, j, runInput)

	cltest.WaitForJobRunStatus(t, app.Store, jr, models.RunStatusInProgress)
	cltest.JobRunStays(t, app.Store, jr, models.RunStatusInProgress, 3*time.Second)
	cltest.WaitForJobRunToComplete(t, app.Store, jr)
}

func TestIntegration_ExternalInitiator(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	require.NoError(t, app.Start())

	exInitr := struct {
		Header http.Header
		Body   web.JobSpecNotice
	}{}
	eiMockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", "",
		func(header http.Header, body string) {
			exInitr.Header = header
			err := json.Unmarshal([]byte(body), &exInitr.Body)
			require.NoError(t, err)
		},
	)
	defer assertCalled()

	eiCreate := map[string]string{
		"name": "someCoin",
		"url":  eiMockServer.URL,
	}
	eiCreateJSON, err := json.Marshal(eiCreate)
	require.NoError(t, err)
	eip := cltest.CreateExternalInitiatorViaWeb(t, app, string(eiCreateJSON))

	eia := &auth.Token{
		AccessKey: eip.AccessKey,
		Secret:    eip.Secret,
	}
	ei, err := app.Store.FindExternalInitiator(eia)
	require.NoError(t, err)

	require.Equal(t, eiCreate["url"], ei.URL.String())
	require.Equal(t, strings.ToLower(eiCreate["name"]), ei.Name)
	require.Equal(t, eip.AccessKey, ei.AccessKey)
	require.Equal(t, eip.OutgoingSecret, ei.OutgoingSecret)

	jobSpec := cltest.FixtureCreateJobViaWeb(t, app, "./testdata/external_initiator_job.json")
	assert.Equal(t,
		eip.OutgoingToken,
		exInitr.Header.Get(web.ExternalInitiatorAccessKeyHeader),
	)
	assert.Equal(t,
		eip.OutgoingSecret,
		exInitr.Header.Get(web.ExternalInitiatorSecretHeader),
	)
	expected := web.JobSpecNotice{
		JobID:  jobSpec.ID,
		Type:   models.InitiatorExternal,
		Params: cltest.JSONFromString(t, `{"foo":"bar"}`),
	}
	assert.Equal(t, expected, exInitr.Body)

	jobRun := cltest.CreateJobRunViaExternalInitiator(t, app, jobSpec, *eia, "")
	_, err = app.Store.JobRunsFor(jobRun.ID)
	assert.NoError(t, err)
	cltest.WaitForJobRunToComplete(t, app.Store, jobRun)
}

func TestIntegration_ExternalInitiator_WithoutURL(t *testing.T) {
	t.Parallel()

	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()
	require.NoError(t, app.Start())

	eiCreate := map[string]string{
		"name": "someCoin",
	}
	eiCreateJSON, err := json.Marshal(eiCreate)
	require.NoError(t, err)
	eip := cltest.CreateExternalInitiatorViaWeb(t, app, string(eiCreateJSON))

	eia := &auth.Token{
		AccessKey: eip.AccessKey,
		Secret:    eip.Secret,
	}
	ei, err := app.Store.FindExternalInitiator(eia)
	require.NoError(t, err)

	require.Equal(t, strings.ToLower(eiCreate["name"]), ei.Name)
	require.Equal(t, eip.AccessKey, ei.AccessKey)
	require.Equal(t, eip.OutgoingSecret, ei.OutgoingSecret)

	jobSpec := cltest.FixtureCreateJobViaWeb(t, app, "./testdata/external_initiator_job.json")

	jobRun := cltest.CreateJobRunViaExternalInitiator(t, app, jobSpec, *eia, "")
	_, err = app.Store.JobRunsFor(jobRun.ID)
	assert.NoError(t, err)
	cltest.WaitForJobRunToComplete(t, app.Store, jobRun)
}

func TestIntegration_AuthToken(t *testing.T) {
	app, cleanup := cltest.NewApplication(t,
		cltest.LenientEthMock,
		cltest.EthMockRegisterChainID,
		cltest.EthMockRegisterGetBalance,
	)
	defer cleanup()

	require.NoError(t, app.Start())

	// set up user
	mockUser := cltest.MustRandomUser()
	apiToken := auth.Token{AccessKey: cltest.APIKey, Secret: cltest.APISecret}
	require.NoError(t, mockUser.SetAuthToken(&apiToken))
	require.NoError(t, app.Store.SaveUser(&mockUser))

	url := app.Config.ClientNodeURL() + "/v2/config"
	headers := make(map[string]string)
	headers[web.APIKey] = cltest.APIKey
	headers[web.APISecret] = cltest.APISecret
	buf := bytes.NewBufferString(`{"ethGasPriceDefault":15000000}`)

	resp, cleanup := cltest.UnauthenticatedPatch(t, url, buf, headers)
	defer cleanup()
	cltest.AssertServerResponse(t, resp, http.StatusOK)
}

func TestIntegration_FluxMonitor_Deviation(t *testing.T) {
	gethClient := new(mocks.GethClient)
	rpcClient := new(mocks.RPCClient)
	sub := new(mocks.Subscription)

	config, cfgCleanup := cltest.NewConfig(t)
	defer cfgCleanup()
	app, appCleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		eth.NewClientWith(rpcClient, gethClient),
	)
	defer appCleanup()

	// Start, connect, and initialize node
	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return(nil).Maybe()
	gethClient.On("ChainID", mock.Anything).Return(app.Store.Config.ChainID(), nil)
	gethClient.On("BalanceAt", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(oneETH.ToInt(), nil)
	chchNewHeads := make(chan chan<- *models.Head, 1)
	rpcClient.On("EthSubscribe", mock.Anything, mock.Anything, "newHeads").
		Run(func(args mock.Arguments) { chchNewHeads <- args.Get(1).(chan<- *models.Head) }).
		Return(sub, nil)

	logsSub := new(mocks.Subscription)
	logsSub.On("Err").Return(nil)
	logsSub.On("Unsubscribe").Return(nil).Maybe()

	err := app.StartAndConnect()
	require.NoError(t, err)

	gethClient.AssertExpectations(t)
	rpcClient.AssertExpectations(t)
	sub.AssertExpectations(t)

	// Configure fake Eth Node to return 10,000 cents when FM initiates price.
	minPayment := app.Store.Config.MinimumContractPayment().ToInt().Uint64()
	availableFunds := minPayment * 100
	rpcClient.On("Call", mock.Anything, "eth_call", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*args.Get(0).(*hexutil.Bytes) = cltest.MakeRoundStateReturnData(2, true, 10000, 7, 0, availableFunds, minPayment, 1)
		}).
		Return(nil).
		Once()

	// Have server respond with 102 for price when FM checks external price
	// adapter for deviation. 102 is enough deviation to trigger a job run.
	priceResponse := `{"data":{"result": 102}}`
	mockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", priceResponse)
	defer assertCalled()

	confirmed := int64(23456)
	safe := confirmed + int64(config.MinRequiredOutgoingConfirmations())
	inLongestChain := safe - int64(config.GasUpdaterBlockDelay())

	// Single task ethTx receives configuration from FM init and writes to chain.
	gethClient.On("SubscribeFilterLogs", mock.Anything, mock.Anything, mock.Anything).
		Return(logsSub, nil)
	gethClient.On("FilterLogs", mock.Anything, mock.Anything).Return([]models.Log{}, nil)

	// Initial tx attempt sent
	gethClient.On("SendTransaction", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			tx, ok := args.Get(1).(*types.Transaction)
			require.True(t, ok)
			gethClient.On("TransactionReceipt", mock.Anything, mock.Anything).
				Return(&types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(confirmed)}, nil)
		}).
		Return(nil).Once()

	rpcClient.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(inLongestChain)
		}).
		Return(nil)

	gethClient.On("BlockByNumber", mock.Anything, big.NewInt(inLongestChain)).Return(cltest.BlockWithTransactions(), nil)

	// Create FM Job, and wait for job run to start because the above criteria initiates a run.
	buffer := cltest.MustReadFile(t, "testdata/flux_monitor_job.json")
	var job models.JobSpec
	err = json.Unmarshal(buffer, &job)
	require.NoError(t, err)
	job.Initiators[0].InitiatorParams.Feeds = cltest.JSONFromString(t, fmt.Sprintf(`["%s"]`, mockServer.URL))
	job.Initiators[0].InitiatorParams.PollTimer.Period = models.MustMakeDuration(15 * time.Second)

	j := cltest.CreateJobSpecViaWeb(t, app, job)
	jrs := cltest.WaitForRuns(t, j, app.Store, 1)
	jr := cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jrs[0])
	cltest.WaitForEthTxAttemptCount(t, app.Store, 1)

	newHeads := <-chchNewHeads
	newHeads <- cltest.Head(safe)

	// Check the FM price on completed run output
	jr = cltest.WaitForJobRunToComplete(t, app.GetStore(), jr)

	requestParams := jr.RunRequest.RequestParams
	assert.Equal(t, "102", requestParams.Get("result").String())
	assert.Equal(
		t,
		"0x3cCad4715152693fE3BC4460591e3D3Fbd071b42", // from testdata/flux_monitor_job.json
		requestParams.Get("address").String())
	assert.Equal(t, "0x202ee0ed", requestParams.Get("functionSelector").String())
	assert.Equal(
		t,
		"0x0000000000000000000000000000000000000000000000000000000000000002",
		requestParams.Get("dataPrefix").String())

	linkEarned, err := app.GetStore().LinkEarnedFor(&j)
	require.NoError(t, err)
	assert.Equal(t, app.Store.Config.MinimumContractPayment(), linkEarned)

	gethClient.AssertExpectations(t)
	rpcClient.AssertExpectations(t)
	sub.AssertExpectations(t)
}

func TestIntegration_FluxMonitor_NewRound(t *testing.T) {
	gethClient := new(mocks.GethClient)
	rpcClient := new(mocks.RPCClient)
	sub := new(mocks.Subscription)

	config, cleanup := cltest.NewConfig(t)
	defer cleanup()
	app, cleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		eth.NewClientWith(rpcClient, gethClient),
	)
	defer cleanup()

	app.GetStore().Config.Set(orm.EnvVarName("MinRequiredOutgoingConfirmations"), 1)
	minPayment := app.Store.Config.MinimumContractPayment().ToInt().Uint64()
	availableFunds := minPayment * 100

	// Start, connect, and initialize node
	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return(nil).Maybe()
	gethClient.On("ChainID", mock.Anything).Return(app.Store.Config.ChainID(), nil)
	gethClient.On("BalanceAt", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(oneETH.ToInt(), nil)
	chchNewHeads := make(chan chan<- *models.Head, 1)
	rpcClient.On("EthSubscribe", mock.Anything, mock.Anything, "newHeads").
		Run(func(args mock.Arguments) { chchNewHeads <- args.Get(1).(chan<- *models.Head) }).
		Return(sub, nil)

	err := app.StartAndConnect()
	require.NoError(t, err)

	gethClient.AssertExpectations(t)
	rpcClient.AssertExpectations(t)
	sub.AssertExpectations(t)

	// Configure fake Eth Node to return 10,000 cents when FM initiates price.
	rpcClient.On("Call", mock.Anything, "eth_call", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*args.Get(0).(*hexutil.Bytes) = cltest.MakeRoundStateReturnData(2, true, 10000, 7, 0, availableFunds, minPayment, 1)
		}).
		Return(nil)

	// Have price adapter server respond with 100 for price on initialization,
	// NOT enough for deviation.
	priceResponse := `{"data":{"result": 100}}`
	mockServer, assertCalled := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", priceResponse)
	defer assertCalled()

	confirmed := int64(23456)
	safe := confirmed + int64(config.MinRequiredOutgoingConfirmations())
	inLongestChain := safe - int64(config.GasUpdaterBlockDelay())

	// Prepare new rounds logs subscription to be called by new FM job
	chchLogs := make(chan chan<- types.Log, 1)
	gethClient.On("SubscribeFilterLogs", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { chchLogs <- args.Get(2).(chan<- types.Log) }).
		Return(sub, nil)

	// Log Broadcaster backfills logs
	rpcClient.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(1)
		}).
		Return(nil)
	gethClient.On("FilterLogs", mock.Anything, mock.Anything).Return([]models.Log{}, nil)

	// Create FM Job, and ensure no runs because above criteria has no deviation.
	buffer := cltest.MustReadFile(t, "testdata/flux_monitor_job.json")
	var job models.JobSpec
	err = json.Unmarshal(buffer, &job)
	require.NoError(t, err)
	job.Initiators[0].InitiatorParams.Feeds = cltest.JSONFromString(t, fmt.Sprintf(`["%s"]`, mockServer.URL))
	job.Initiators[0].InitiatorParams.PollTimer.Period = models.MustMakeDuration(15 * time.Second)
	job.Initiators[0].InitiatorParams.IdleTimer.Disabled = true
	job.Initiators[0].InitiatorParams.IdleTimer.Duration = models.MustMakeDuration(0)

	j := cltest.CreateJobSpecViaWeb(t, app, job)
	_ = cltest.AssertRunsStays(t, j, app.Store, 0)

	gethClient.AssertExpectations(t)
	rpcClient.AssertExpectations(t)
	sub.AssertExpectations(t)

	// Send a NewRound log event to trigger a run.
	log := cltest.LogFromFixture(t, "testdata/new_round_log.json")
	log.Address = job.Initiators[0].InitiatorParams.Address

	gethClient.On("SendTransaction", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			tx, ok := args.Get(1).(*types.Transaction)
			require.True(t, ok)
			gethClient.On("TransactionReceipt", mock.Anything, mock.Anything).
				Return(&types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(confirmed)}, nil)
		}).
		Return(nil).Once()

	rpcClient.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(inLongestChain)
		}).
		Return(nil)

	gethClient.On("BlockByNumber", mock.Anything, big.NewInt(inLongestChain)).Return(cltest.BlockWithTransactions(), nil)

	// Flux Monitor queries FluxAggregator.RoundState()
	rpcClient.On("Call", mock.Anything, "eth_call", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*args.Get(1).(*hexutil.Bytes) = cltest.MakeRoundStateReturnData(2, true, 10000, 7, 0, availableFunds, minPayment, 1)
		}).
		Return(nil)

	newRounds := <-chchLogs
	newRounds <- log
	jrs := cltest.WaitForRuns(t, j, app.Store, 1)
	_ = cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jrs[0])
	cltest.WaitForEthTxAttemptCount(t, app.Store, 1)

	newHeads := <-chchNewHeads
	newHeads <- cltest.Head(safe)
	_ = cltest.WaitForJobRunToComplete(t, app.Store, jrs[0])
	linkEarned, err := app.GetStore().LinkEarnedFor(&j)
	require.NoError(t, err)
	assert.Equal(t, app.Store.Config.MinimumContractPayment(), linkEarned)

	gethClient.AssertExpectations(t)
	rpcClient.AssertExpectations(t)
	sub.AssertExpectations(t)
}

// TestIntegration_EthTX_Reconnect tests that JobRuns that are interrupted due to
// eth client connection issues are re-started appropriately. In particular, they
// should broadcast a tx with the result of the original RunInput.

// TODO - remove this test when removing the Legacy TXM - it is redundant
func TestIntegration_EthTX_Reconnect(t *testing.T) {
	t.Parallel()

	gethClient := new(mocks.GethClient)
	rpcClient := new(mocks.RPCClient)
	sub := new(mocks.Subscription)

	config, cfgCleanup := cltest.NewConfig(t)
	defer cfgCleanup()
	config.Set("MIN_OUTGOING_CONFIRMATIONS", 1)
	app, appCleanup := cltest.NewApplicationWithConfigAndKey(t, config,
		eth.NewClientWith(rpcClient, gethClient),
	)
	defer appCleanup()

	confirmed := int64(23456)
	safe := confirmed + int64(config.MinRequiredOutgoingConfirmations())
	inLongestChain := safe - int64(config.GasUpdaterBlockDelay())

	// Start, connect, and initialize node
	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return(nil).Maybe()
	gethClient.On("ChainID", mock.Anything).Return(app.Store.Config.ChainID(), nil)
	gethClient.On("BalanceAt", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(oneETH.ToInt(), nil)
	chchNewHeads := make(chan chan<- *models.Head, 1)
	rpcClient.On("EthSubscribe", mock.Anything, mock.Anything, "newHeads").
		Run(func(args mock.Arguments) { chchNewHeads <- args.Get(1).(chan<- *models.Head) }).
		Return(sub, nil)
	rpcClient.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(inLongestChain)
		}).
		Return(nil)

	gethClient.On("BlockByNumber", mock.Anything, big.NewInt(inLongestChain)).Return(cltest.BlockWithTransactions(), nil)

	gethClient.On("SendTransaction", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			tx, ok := args.Get(1).(*types.Transaction)
			require.True(t, ok)
			gethClient.On("TransactionReceipt", mock.Anything, mock.Anything).
				Return(&types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(confirmed)}, nil)
		}).
		Return(nil).Once()

	require.NoError(t, app.StartAndConnect())

	j := cltest.FixtureCreateJobViaWeb(t, app, "fixtures/web/web_initiated_eth_tx_job.json")
	result := "0x11"
	jr := cltest.CreateJobRunViaWeb(t, app, j, fmt.Sprintf(`{"result":"%v"}`, result))
	cltest.WaitForJobRunToPendOutgoingConfirmations(t, app.Store, jr)
	cltest.WaitForEthTxAttemptCount(t, app.Store, 1)

	newHeads := <-chchNewHeads
	newHeads <- cltest.Head(safe)

	cltest.WaitForJobRunToComplete(t, app.Store, jr)

	tx := cltest.GetLastEthTx(t, app.Store)
	resultOnChain := hexutil.Encode(common.TrimLeftZeroes(tx.EncodedPayload))

	assert.Equal(t, result, resultOnChain)
}
