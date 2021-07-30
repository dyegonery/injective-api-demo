# go-sdk-demo

|   Author   |           Email           |
|------------|---------------------------|
|Po Wen Perng|powen@injectiveprotocol.com|

## Prerequisite
go 1.16+

## How to run demo
```bash
$ cd /path/to/sdk_go
$ go run example.go
```

This demo will show some examples for using go-sdk.

In the beginning of example.go, it will set up all the parameter to connect to test environment.

The full well done code of environment setting is in [injective-api-demo](https://github.com/InjectiveLabs/injective-api-demo/tree/master/go).

### 1. getting all market infos
```go
    respMarkets, errMarkets := spotsClient.Markets(ctx, &spotExchangePB.MarketsRequest{})
	if errMarkets != nil || respMarkets == nil {
		log.Infof("Failed to get spot markets")
	}
```

### 2. getting balances
```go
    respBalance, errBalance := accountsClient.SubaccountBalancesList(ctx, &accountsPB.SubaccountBalancesListRequest{
        SubaccountId: subaccountID.Hex(),
    })
    if errBalance != nil {
        log.Errorln("Failed getting balances with err: ", err.Error())
    }
```

### 3. gett all orders
```go
    respOrders, errOrders := spotsClient.Orders(ctx, &spotExchangePB.OrdersRequest{
        SubaccountId: subaccountID.Hex(),
        MarketId:     market.MarketId,
    })
    if errOrders != nil {
        log.Infof("Failed to get orders")
    }
```

### 4. canceling order / orders
```go
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
```

### 5. posting order / orders
```go
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
```
## Note
There are lots of decimal conversion between cosmostype and normal type.

This demo also including how to go the conversion, for example:

```go
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
```