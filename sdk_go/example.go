package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	log "github.com/xlab/suplog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	chainclient "github.com/InjectiveLabs/sdk-go/chain/client"
	"github.com/InjectiveLabs/sdk-go/chain/crypto/ethsecp256k1"
	"github.com/InjectiveLabs/sdk-go/chain/crypto/hd"
	exchangetypes "github.com/InjectiveLabs/sdk-go/chain/exchange/types"
	accountsPB "github.com/InjectiveLabs/sdk-go/exchange/accounts_rpc/pb"
	spotExchangePB "github.com/InjectiveLabs/sdk-go/exchange/spot_exchange_rpc/pb"
	cosmcrypto "github.com/cosmos/cosmos-sdk/crypto"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmtypes "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
)

const defaultKeyringKeyName = "validator"

var emptyCosmosAddress = cosmtypes.AccAddress{}

func main() {
	// initial all the parameter first
	var (
		// Cosmos params
		cosmosChainID   string = "injective-1"
		cosmosGRPC      string = "tcp://sentry0.injective.network:9900"
		tendermintRPC   string = "http://sentry0.injective.network:26657"
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
		exchangeGRPC string = "tcp://sentry0.injective.network:9910"
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

	// set up api clients
	accountsClient := accountsPB.NewInjectiveAccountsRPCClient(exchangeConn)
	spotsClient := spotExchangePB.NewInjectiveSpotExchangeRPCClient(exchangeConn)
	cosmosClient := daemonClient
	bankQueryClient := banktypes.NewQueryClient(daemonConn)

	//demo go-sdk part
	ctx, _ := context.WithCancel(context.Background())

	//deposit all balances from inj chain to trading subaccount
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
		// this is the way to execute msgs (sync mode)
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
				MarketId:  market.MarketId,
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

// utility functions below

func initCosmosKeyring(
	cosmosKeyringDir *string,
	cosmosKeyringAppName *string,
	cosmosKeyringBackend *string,
	cosmosKeyFrom *string,
	cosmosKeyPassphrase *string,
	cosmosPrivKey *string,
	cosmosUseLedger *bool,
) (cosmtypes.AccAddress, keyring.Keyring, error) {
	switch {
	case len(*cosmosPrivKey) > 0:
		if *cosmosUseLedger {
			err := errors.New("cannot combine ledger and privkey options")
			return emptyCosmosAddress, nil, err
		}

		pkBytes, err := hexToBytes(*cosmosPrivKey)
		if err != nil {
			err = errors.Wrap(err, "failed to hex-decode cosmos account privkey")
			return emptyCosmosAddress, nil, err
		}

		// Specfic to Injective chain with Ethermint keys
		// Should be secp256k1.PrivKey for generic Cosmos chain
		cosmosAccPk := &ethsecp256k1.PrivKey{
			Key: pkBytes,
		}

		addressFromPk := cosmtypes.AccAddress(cosmosAccPk.PubKey().Address().Bytes())

		var keyName string

		// check that if cosmos 'From' specified separately, it must match the provided privkey,
		if len(*cosmosKeyFrom) > 0 {
			addressFrom, err := cosmtypes.AccAddressFromBech32(*cosmosKeyFrom)
			if err == nil {
				if !bytes.Equal(addressFrom.Bytes(), addressFromPk.Bytes()) {
					err = errors.Errorf("expected account address %s but got %s from the private key", addressFrom.String(), addressFromPk.String())
					return emptyCosmosAddress, nil, err
				}
			} else {
				// use it as a name then
				keyName = *cosmosKeyFrom
			}
		}

		if len(keyName) == 0 {
			keyName = defaultKeyringKeyName
		}

		// wrap a PK into a Keyring
		kb, err := KeyringForPrivKey(keyName, cosmosAccPk)
		return addressFromPk, kb, err

	case len(*cosmosKeyFrom) > 0:
		var fromIsAddress bool
		addressFrom, err := cosmtypes.AccAddressFromBech32(*cosmosKeyFrom)
		if err == nil {
			fromIsAddress = true
		}

		var passReader io.Reader = os.Stdin
		if len(*cosmosKeyPassphrase) > 0 {
			passReader = newPassReader(*cosmosKeyPassphrase)
		}

		var absoluteKeyringDir string
		if filepath.IsAbs(*cosmosKeyringDir) {
			absoluteKeyringDir = *cosmosKeyringDir
		} else {
			absoluteKeyringDir, _ = filepath.Abs(*cosmosKeyringDir)
		}

		kb, err := keyring.New(
			*cosmosKeyringAppName,
			*cosmosKeyringBackend,
			absoluteKeyringDir,
			passReader,
			hd.EthSecp256k1Option(),
		)
		if err != nil {
			err = errors.Wrap(err, "failed to init keyring")
			return emptyCosmosAddress, nil, err
		}

		var keyInfo keyring.Info
		if fromIsAddress {
			if keyInfo, err = kb.KeyByAddress(addressFrom); err != nil {
				err = errors.Wrapf(err, "couldn't find an entry for the key %s in keybase", addressFrom.String())
				return emptyCosmosAddress, nil, err
			}
		} else {
			if keyInfo, err = kb.Key(*cosmosKeyFrom); err != nil {
				err = errors.Wrapf(err, "could not find an entry for the key '%s' in keybase", *cosmosKeyFrom)
				return emptyCosmosAddress, nil, err
			}
		}

		switch keyType := keyInfo.GetType(); keyType {
		case keyring.TypeLocal:
			// kb has a key and it's totally usable
			return keyInfo.GetAddress(), kb, nil
		case keyring.TypeLedger:
			// the kb stores references to ledger keys, so we must explicitly
			// check that. kb doesn't know how to scan HD keys - they must be added manually before
			if *cosmosUseLedger {
				return keyInfo.GetAddress(), kb, nil
			}
			err := errors.Errorf("'%s' key is a ledger reference, enable ledger option", keyInfo.GetName())
			return emptyCosmosAddress, nil, err
		case keyring.TypeOffline:
			err := errors.Errorf("'%s' key is an offline key, not supported yet", keyInfo.GetName())
			return emptyCosmosAddress, nil, err
		case keyring.TypeMulti:
			err := errors.Errorf("'%s' key is an multisig key, not supported yet", keyInfo.GetName())
			return emptyCosmosAddress, nil, err
		default:
			err := errors.Errorf("'%s' key  has unsupported type: %s", keyInfo.GetName(), keyType)
			return emptyCosmosAddress, nil, err
		}

	default:
		err := errors.New("insufficient cosmos key details provided")
		return emptyCosmosAddress, nil, err
	}
}

func newPassReader(pass string) io.Reader {
	return &passReader{
		pass: pass,
		buf:  new(bytes.Buffer),
	}
}

type passReader struct {
	pass string
	buf  *bytes.Buffer
}

var _ io.Reader = &passReader{}

func (r *passReader) Read(p []byte) (n int, err error) {
	n, err = r.buf.Read(p)
	if err == io.EOF || n == 0 {
		r.buf.WriteString(r.pass + "\n")

		n, err = r.buf.Read(p)
	}

	return
}

// KeyringForPrivKey creates a temporary in-mem keyring for a PrivKey.
// Allows to init Context when the key has been provided in plaintext and parsed.
func KeyringForPrivKey(name string, privKey cryptotypes.PrivKey) (keyring.Keyring, error) {
	kb := keyring.NewInMemory(hd.EthSecp256k1Option())
	tmpPhrase := randPhrase(64)
	armored := cosmcrypto.EncryptArmorPrivKey(privKey, tmpPhrase, privKey.Type())
	err := kb.ImportPrivKey(name, armored, tmpPhrase)
	if err != nil {
		err = errors.Wrap(err, "failed to import privkey")
		return nil, err
	}

	return kb, nil
}

func randPhrase(size int) string {
	buf := make([]byte, size)
	_, err := rand.Read(buf)
	orPanic(err)

	return string(buf)
}

func orPanic(err error) {
	if err != nil {
		log.Panicln()
	}
}

func waitForService(ctx context.Context, conn *grpc.ClientConn) error {
	for {
		select {
		case <-ctx.Done():
			return errors.Errorf("Service wait timed out. Please run injective exchange service:\n\nmake install && injective-exchange")
		default:
			state := conn.GetState()

			if state != connectivity.Ready {
				time.Sleep(time.Second)
				continue
			}

			return nil
		}
	}
}

func grpcDialEndpoint(protoAddr string) (conn *grpc.ClientConn, err error) {
	conn, err = grpc.Dial(protoAddr, grpc.WithInsecure(), grpc.WithContextDialer(dialerFunc))
	if err != nil {
		err := errors.Wrapf(err, "failed to connect to the gRPC: %s", protoAddr)
		return nil, err
	}

	return conn, nil
}

func hexToBytes(str string) ([]byte, error) {
	if strings.HasPrefix(str, "0x") {
		str = str[2:]
	}

	data, err := hex.DecodeString(str)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func dialerFunc(ctx context.Context, protoAddr string) (net.Conn, error) {
	proto, address := protocolAndAddress(protoAddr)
	conn, err := net.Dial(proto, address)
	return conn, err
}

func protocolAndAddress(listenAddr string) (string, string) {
	protocol, address := "tcp", listenAddr
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		protocol, address = parts[0], parts[1]
	}
	return protocol, address
}
