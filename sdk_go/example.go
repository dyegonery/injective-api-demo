package main

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/shopspring/decimal"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	log "github.com/xlab/suplog"

	chainclient "github.com/InjectiveLabs/sdk-go/chain/client"
	exchangetypes "github.com/InjectiveLabs/sdk-go/chain/exchange/types"
	accountsPB "github.com/InjectiveLabs/sdk-go/exchange/accounts_rpc/pb"
	spotExchangePB "github.com/InjectiveLabs/sdk-go/exchange/spot_exchange_rpc/pb"
	cosmtypes "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
)

func main() {
	// initial all the parameter first
	var (
		// Cosmos params
		cosmosChainID   string = "injective-1"
		cosmosGRPC      string = "tcp://localhost:9900"
		tendermintRPC   string = "http://localhost:26657"
		cosmosGasPrices string = "500000000inj"

		// Cosmos Key Management
		cosmosKeyringDir     string = ""
		cosmosKeyringAppName string = "injectived"
		cosmosKeyringBackend string = "file"

		cosmosKeyFrom       string = ""
		cosmosKeyPassphrase string = ""
		cosmosPrivKey       string = "" // need private key for this
		cosmosUseLedger     bool   = false

		// Exchange API params
		exchangeGRPC string = "tcp://localhost:9910"

		// Metrics
		statsdPrefix   string = "trading"
		statsdAddr     string = "localhost:8125"
		statsdStuckDur string = "5m"
		statsdMocking  string = "false"
		statsdDisabled string = "true"
	)

	startMetricsGathering(
		&statsdPrefix,
		&statsdAddr,
		&statsdStuckDur,
		&statsdMocking,
		&statsdDisabled,
	)

	senderAddress, cosmosKeyring, err := initCosmosKeyring(
		&cosmosKeyringDir,
		&cosmosKeyringAppName,
		&cosmosKeyringBackend,
		&cosmosKeyFrom,
		&cosmosKeyPassphrase,
		&cosmosPrivKey,
		&cosmosUseLedger,
	)
	if err != nil {
		log.WithError(err).Fatalln("failed to init Cosmos keyring")
	}

	log.Infoln("Using Cosmos Sender", senderAddress.String())

	clientCtx, err := chainclient.NewClientContext(cosmosChainID, senderAddress.String(), cosmosKeyring)
	if err != nil {
		log.WithError(err).Fatalln("failed to initialize cosmos client context")
	}
	clientCtx = clientCtx.WithNodeURI(tendermintRPC)
	tmRPC, err := rpchttp.New(tendermintRPC, "/websocket")
	if err != nil {
		log.WithError(err).Fatalln("failed to connect to tendermint RPC")
	}
	clientCtx = clientCtx.WithClient(tmRPC)

	daemonClient, err := chainclient.NewCosmosClient(clientCtx, cosmosGRPC, chainclient.OptionGasPrices(cosmosGasPrices))
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"endpoint": cosmosGRPC,
		}).Fatalln("failed to connect to daemon, is injectived running?")
	}
	defer daemonClient.Close()

	log.Infoln("Waiting for GRPC services")
	time.Sleep(1 * time.Second)

	daemonWaitCtx, cancelWait := context.WithTimeout(context.Background(), time.Minute)
	daemonConn := daemonClient.QueryClient()
	waitForService(daemonWaitCtx, daemonConn)
	cancelWait()

	exchangeWaitCtx, cancelWait := context.WithTimeout(context.Background(), time.Minute)
	exchangeConn, err := grpcDialEndpoint(exchangeGRPC)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"endpoint": exchangeGRPC,
		}).Fatalln("failed to connect to API, is injective-exchange running?")
	}
	waitForService(exchangeWaitCtx, exchangeConn)
	cancelWait()

	// set up clients
	accountsClient := accountsPB.NewInjectiveAccountsRPCClient(exchangeConn)
	spotsClient := spotExchangePB.NewInjectiveSpotExchangeRPCClient(exchangeConn)
	cosmosClient := chainclient.cosmosClient
	bankQueryClient := banktypes.NewQueryClient(daemonConn)

	//demo go-sdk part
	ctx, _ := context.WithCancel(context.Background())

	//deposit all balances
	sender := cosmosClient.FromAddress()
	resp, err := bankQueryClient.AllBalances(ctx, &banktypes.QueryAllBalancesRequest{
		Address:    sender.String(),
		Pagination: nil,
	})
	if err != nil {
		panic("Need bank balances")
	}
	msgs := make([]cosmtypes.Msg, 0)
	// get subaccountID
	subaccountID := defaultSubaccount(sender)

	log.Infoln("Preparing Injective Chain funds for deposit into exchange subaccount.")

	for _, balance := range resp.Balances {
		if balance.IsZero() {
			continue
		}
		// never let INJ balance go under 100
		if balance.Denom == "inj" {
			minINJAmount, _ := cosmtypes.NewIntFromString("200000000000000000000")
			if balance.Amount.LT(minINJAmount) {
				continue
			} else {
				balance.Amount = balance.Amount.Sub(minINJAmount)
			}
		}
		log.Infof("%s:\t%s \t %s\n", balance.Denom, balance.Amount.String(), subaccountID.Hex())
		msg := exchangetypes.MsgDeposit{
			Sender:       sender.String(),
			SubaccountId: subaccountID.Hex(),
			Amount:       balance,
		}
		msgs = append(msgs, &msg)
	}
	if len(msgs) > 0 {
		// this is the way to execute msgs
		if _, err := cosmosClient.SyncBroadcastMsg(msgs...); err != nil {
			log.Errorln("Failed depositing to exchange with err: ", err.Error())
		}
	}

	//  get market info
	respMarkets, errMarkets := spotsClient.Markets(ctx, &spotExchangePB.MarketsRequest{})
	if errMarkets != nil || respMarkets == nil {
		log.Infof("Failed to get spot markets")
	}
	for _, market := range respMarkets.Markets {
		// ticker ex: INJ/USDT
		// info contains base and quote's denom, and market id
		if market.Ticker == "INJ/USDT" {
			fmt.Println(market.Ticker, market)

			// collect market info
			var baseDenomDecimals, quoteDenomDecimals int
			if market.BaseTokenMeta != nil {
				baseDenomDecimals = int(market.BaseTokenMeta.Decimals)
			} else {
				baseDenomDecimals = 18
			}
			if market.QuoteTokenMeta != nil {
				quoteDenomDecimals = int(market.QuoteTokenMeta.Decimals)
			} else {
				quoteDenomDecimals = 6
			}
			minPriceTickSize, _ := cosmtypes.NewDecFromStr(market.MinPriceTickSize)
			minQuantityTickSize, _ := cosmtypes.NewDecFromStr(market.MinQuantityTickSize)

			// get balances
			respBalance, errBalance := accountsClient.SubaccountBalancesList(ctx, &accountsPB.SubaccountBalancesListRequest{
				SubaccountId: subaccountID.Hex(),
			})
			if errBalance != nil {
				log.Errorln("Failed getting balances with err: ", err.Error())
			}
			for _, balance := range respBalance.Balances {
				switch balance.Denom {
				case market.BaseDenom:
					fmt.Println("INJ:", balance.Deposit.AvailableBalance)
					avaiB, _ := decimal.NewFromString(balance.Deposit.AvailableBalance)
					// trans back into readable decimal
					deciAvaiB := decimal.NewFromBigInt(avaiB.BigInt(), int32(-baseDenomDecimals))
					fmt.Println("INJ:", deciAvaiB)

				case market.QuoteDenom:
					fmt.Println("USDT", balance.Deposit.TotalBalance)
					totalQ, _ := decimal.NewFromString(balance.Deposit.TotalBalance)
					deciTotalB := decimal.NewFromBigInt(totalQ.BigInt(), int32(-quoteDenomDecimals))
					fmt.Println("USDT", deciTotalB)
				}
			}

			// get orders
			respOrders, errOrders := spotsClient.Orders(ctx, &spotExchangePB.OrdersRequest{
				SubaccountId: subaccountID.Hex(),
				MarketId:     market.MarketId,
			})
			if errOrders != nil {
				log.Infof("Failed to get orders")
			}

			// cancel all orders
			msgs := make([]cosmtypes.Msg, 0)
			for _, order := range respOrders.Orders {
				msg := &exchangetypes.MsgCancelSpotOrder{
					Sender:       sender.String(),
					MarketId:     market.MarketId,
					SubaccountId: subaccountID.Hex(),
					OrderHash:    order.OrderHash,
				}
				msgs = append(msgs, msg)

				// print order price and quantity
				fmt.Println(order.Price, order.Quantity)
			}
			// use msgs slice to cancel one order or multi orders
			if len(msgs) > 0 {
				if _, err := cosmosClient.SyncBroadcastMsg(msgs...); err != nil {
					log.Errorln("Failed canceling orders with err", err.Error())
				}
			}

			// posting order, buy 50 inj at 10 usdt
			price := decimal.NewFromInt(10)
			size := decimal.NewFromInt(50)

			// need to trans decimal.Decimal type into cosmostype.Dec fitting the chain
			cosPrice := getPrice(price, baseDenomDecimals, quoteDenomDecimals, minPriceTickSize)
			cosSize := getQuantity(size, minQuantityTickSize, baseDenomDecimals)

			// preparing new order, orderType 1 (buy), 2 (sell)
			msgs = make([]cosmtypes.Msg, 0)
			newOrder := &exchangetypes.SpotOrder{
				MarketId:  market.MarketID.Hex(),
				OrderType: 1,
				OrderInfo: exchangetypes.OrderInfo{
					SubaccountId: subaccountID.Hex(),
					FeeRecipient: sender.String(),
					Price:        cosPrice,
					Quantity:     cosSize,
				},
			}
			msg := &exchangetypes.MsgCreateSpotLimitOrder{
				Sender: sender.String(),
				Order:  *newOrder,
			}
			msgs = append(msgs, msg)
			// use msgs slice to post one order or multi orders
			if len(msgs) > 0 {
				if _, err := cosmosClient.SyncBroadcastMsg(msgs...); err != nil {
					log.Errorln("Failed posting orders with err", err.Error())
				}
			}
		}
	}
}

func defaultSubaccount(acc cosmtypes.AccAddress) common.Hash {
	return common.BytesToHash(common.RightPadBytes(acc.Bytes(), 32))
}

// price * 10^quoteDecimals/10^baseDecimals = price * 10^(quoteDecimals - baseDecimals)
// for INJ/USDT, INJ is the base which has 18 decimals and USDT is the quote which has 6 decimals
func getPrice(price decimal.Decimal, baseDecimals, quoteDecimals int, minPriceTickSize cosmtypes.Dec) cosmtypes.Dec {
	scale := decimal.New(1, int32(quoteDecimals-baseDecimals))
	priceStr := scale.Mul(price).StringFixed(18)
	decPrice, err := cosmtypes.NewDecFromStr(priceStr)
	if err != nil {
		fmt.Println(err.Error())
		fmt.Println(priceStr, scale.String(), price.String())
		fmt.Println(decPrice.String())
	}
	residue := new(big.Int).Mod(decPrice.BigInt(), minPriceTickSize.BigInt())
	formattedPrice := new(big.Int).Sub(decPrice.BigInt(), residue)
	p := decimal.NewFromBigInt(formattedPrice, -18).String()
	realPrice, _ := cosmtypes.NewDecFromStr(p)
	return realPrice
}

// convert decimal.Decimal into acceptable quantity, (input value's unit is coin, ex: 5 inj)
func getQuantity(value decimal.Decimal, minTickSize cosmtypes.Dec, baseDecimals int) (qty cosmtypes.Dec) {
	mid, _ := cosmtypes.NewDecFromStr(value.String())
	bStr := decimal.New(1, int32(baseDecimals)).String()
	baseDec, _ := cosmtypes.NewDecFromStr(bStr)
	scale := baseDec.Quo(minTickSize) // from tick size to coin size
	midScaledInt := mid.Mul(scale).TruncateDec()
	qty = minTickSize.Mul(midScaledInt)
	return qty
}
