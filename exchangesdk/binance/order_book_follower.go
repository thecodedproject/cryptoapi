package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/thecodedproject/crypto/exchangesdk"
	"github.com/thecodedproject/crypto/exchangesdk/requestutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	obUrl = "https://api.binance.com/api/v1/depth"

	orderBookStream = "btceur@depth"
	tradesStream = "btceur@trade"
	wsBaseUrl = "wss://stream.binance.com:9443/stream"

	// TODO: Set these in a more robust way
	MARKET_PRICE_PRECISION = 0.01
	MARKET_VOLUME_PRECISION = 1e-8

	WEBSOCKET_LIFETIME = 55*time.Minute
)

type internalOrderBook struct {
	exchangesdk.OrderBook
	lastUpdateId int64
}

func NewMarketFollower(
	ctx context.Context,
	wg *sync.WaitGroup,
	pair exchangesdk.Pair,
) (<-chan exchangesdk.OrderBook, <-chan exchangesdk.OrderBookTrade, error) {

	if pair != exchangesdk.BTCEUR {
		return nil, nil, errors.New("Only BTCEUR is supported")
	}

	return followForever(
		ctx,
		wg,
	)
}

func wsUrl() string {

	// Building the URL with the `url` package (using a values type)
	// seems to cause errors when connecting to the websocket - so
	// doing string manipulation instead
	fullUrl := fmt.Sprintf(
		"%s?streams=%s/%s",
		wsBaseUrl,
		orderBookStream,
		tradesStream,
	)
	return fullUrl
}

func followForever(
	ctx context.Context,
	wg *sync.WaitGroup,
) (<-chan exchangesdk.OrderBook, <-chan exchangesdk.OrderBookTrade, error) {

	obf := make(chan exchangesdk.OrderBook, 1)
	tradeStream := make(chan exchangesdk.OrderBookTrade, 1)
	var ws *websocket.Conn
	wsAge := time.Time{}

	go func() {

		ob, err := getLatestSnapshot()
		if err != nil {
			log.Println("OrderBookFollower error:", err)
			close(obf)
			wg.Done()
			return
		}

		for {
			if wsAge.Before(time.Now().Add(-WEBSOCKET_LIFETIME)) {
				log.Println("New ws!!")
				if ws != nil {
					ws.Close()
				}
				ws, wsAge, err = newWebsocket()
				if err != nil {
					log.Println("OrderBookFollower error:", err)
					close(obf)
					wg.Done()
					return
				}
				defer ws.Close()
			}

			_, msg, err := ws.ReadMessage()
			if err != nil {
				log.Println("OrderBookFollower error:", err)
				close(obf)
				wg.Done()
				return
			}

			update := struct{
				Stream string `json:"stream"`
				Data json.RawMessage `json:"data"`
			}{}

			err = json.Unmarshal(msg, &update)
			if err != nil {
				log.Println("OrderBookFollower error:", err, string(msg))
				close(obf)
				wg.Done()
				return
			}

			switch update.Stream {
			case orderBookStream:
				err := handleOrderBookUpdate(&ob, update.Data)
				if err != nil {
					log.Println("OrderBookFollower error:", err)
					close(obf)
					wg.Done()
					return
				}

				obf <- ob.OrderBook
			case tradesStream:
				trade, err := decodeTrade(update.Data)
				if err != nil {
					log.Println("OrderBookFollower error:", err)
					close(tradeStream)
					wg.Done()
					return
				}
				tradeStream <- trade
			}

			select{
			case <-ctx.Done():
				wg.Done()
				return
			default:
				continue
			}
		}
	}()

	return obf, tradeStream, nil
}

func getLatestSnapshot() (internalOrderBook, error) {

	path := requestutil.FullPath(baseUrl, "api/v3/depth")
	values := url.Values{}
	values.Add("symbol", "BTCEUR")
	values.Add("limit", "1000")
	path.RawQuery = values.Encode()

	body, err := GetBody(http.DefaultClient.Get(path.String()))
	if err != nil {
		return internalOrderBook{}, err
	}

	snapshot := struct{
		LastUpdateId int64 `json:"lastUpdateId"`
		Bids [][]string `json:"bids"`
		Asks [][]string `json:"asks"`
	}{}

	err = json.Unmarshal(body, &snapshot)
	if err != nil {
		return internalOrderBook{}, err
	}

	bids, err := convertOrders(snapshot.Bids)
	if err != nil {
		return internalOrderBook{}, err
	}
	asks, err := convertOrders(snapshot.Asks)
	if err != nil {
		return internalOrderBook{}, err
	}

	ob := internalOrderBook{
		lastUpdateId: snapshot.LastUpdateId,
		OrderBook: exchangesdk.OrderBook{
			Bids: bids,
			Asks: asks,
		},
	}

	err = sortOrderBook(&ob)
	if err != nil {
		return internalOrderBook{}, err
	}

	return ob, nil
}

func handleOrderBookUpdate(ob *internalOrderBook, updateMsg []byte) error {

	update := struct{
		FirstUpdateId int64 `json:"U"`
		LastUpdateId int64 `json:"u"`
		BidUpdates [][]string `json:"b"`
		AskUpdates [][]string `json:"a"`
		Timestamp int64 `json:"E"`
		Temp string `json:"e"`
	}{}

	err := json.Unmarshal(updateMsg, &update)
	if err != nil {
		return err
	}

	if update.LastUpdateId <= ob.lastUpdateId {
		return nil
	}

	if update.LastUpdateId < ob.lastUpdateId+1 &&
			update.FirstUpdateId != ob.lastUpdateId+1 {
		return fmt.Errorf(
			"out of order update; expected updateID %d, got %d",
			ob.lastUpdateId+1,
			update.FirstUpdateId,
		)
	}

	err = UpdateOrders(&ob.Bids, update.BidUpdates)
	if err != nil {
		return err
	}
	err = UpdateOrders(&ob.Asks, update.AskUpdates)
	if err != nil {
		return err
	}

	err = sortOrderBook(ob)
	if err != nil {
		return err
	}

	ob.lastUpdateId = update.LastUpdateId

	ob.Timestamp = time.Unix(0, update.Timestamp * int64(time.Millisecond))

	return nil
}

func decodeTrade(msgData []byte) (exchangesdk.OrderBookTrade, error) {

	tradeJson := struct{
		Price float64 `json:"p,string"`
		Volume float64 `json:"q,string"`
		BuyerIsMaker bool `json:"m,bool"`
		Timestamp int64 `json:"E"`
		Temp2 string `json:"e"`
		Temp bool `json:"M,bool"`
	}{}

	err := json.Unmarshal(msgData, &tradeJson)
	if err != nil {
		return exchangesdk.OrderBookTrade{}, err
	}

	makerSide := exchangesdk.MarketSideSell
	if tradeJson.BuyerIsMaker {
		makerSide = exchangesdk.MarketSideBuy
	}

	return exchangesdk.OrderBookTrade{
		MakerSide: makerSide,
		Price: tradeJson.Price,
		Volume: tradeJson.Volume,
		Timestamp: time.Unix(0, tradeJson.Timestamp * int64(time.Millisecond)),
	}, nil
}

func pricesEqual(a, b exchangesdk.OrderBookOrder) bool {

	return math.Abs(a.Price-b.Price) < (MARKET_PRICE_PRECISION/float64(2))
}

func hasZeroVolume(o exchangesdk.OrderBookOrder) bool {

	return math.Abs(o.Volume) < (MARKET_VOLUME_PRECISION/float64(2))
}

func UpdateOrders(currentOrders *[]exchangesdk.OrderBookOrder, updates [][]string) error {

	for _, update := range updates {

		orderUpdate, err := convertOrderStrings(update)
		if err != nil {
			return err
		}

		foundOrder := false
		for i := range *currentOrders {
			if pricesEqual((*currentOrders)[i], orderUpdate) {
				foundOrder = true

				(*currentOrders)[i].Volume = orderUpdate.Volume

				if hasZeroVolume((*currentOrders)[i]) {
					(*currentOrders)[i] = (*currentOrders)[len(*currentOrders)-1]
					*currentOrders = (*currentOrders)[:len(*currentOrders)-1]
				}

				break
			}
		}

		if !foundOrder && !hasZeroVolume(orderUpdate) {
			*currentOrders = append(*currentOrders, orderUpdate)
		}
	}

	return nil
}

func convertOrders(raw [][]string) ([]exchangesdk.OrderBookOrder, error) {

	orders := make([]exchangesdk.OrderBookOrder, 0, len(raw))
	for _, o := range raw {

		order, err := convertOrderStrings(o)
		if err != nil {
			return nil, err
		}

		orders = append(orders, order)
	}
	return orders, nil
}

func convertOrderStrings(rawOrder []string) (exchangesdk.OrderBookOrder, error) {

	if len(rawOrder) != 2 {
		return exchangesdk.OrderBookOrder{}, fmt.Errorf("Raw order len != 2")
	}

	price, err := strconv.ParseFloat(rawOrder[0], 64)
	if err != nil {
		return exchangesdk.OrderBookOrder{}, err
	}

	volume, err := strconv.ParseFloat(rawOrder[1], 64)
	if err != nil {
		return exchangesdk.OrderBookOrder{}, err
	}

	return exchangesdk.OrderBookOrder{
		Price: price,
		Volume: volume,
	}, nil
}

// DEPRECATED Use exchangesdk.SortOrderBook instead
func sortOrderBook(ob *internalOrderBook) error {

	err := sortOrders(&ob.Bids, sortOrderingDecending)
	if err != nil {
		return err
	}
	err = sortOrders(&ob.Asks, sortOrderingIncrementing)
	if err != nil {
		return err
	}
	return nil
}

type sortOrdering int

const (
	sortOrderingDecending = iota
	sortOrderingIncrementing
	sortOrderingUnknown
)

func sortOrders(orders *[]exchangesdk.OrderBookOrder, ordering sortOrdering) error {


	switch ordering {
	case sortOrderingDecending:
		sort.Slice(*orders, func(i, j int) bool {

			return (*orders)[i].Price > (*orders)[j].Price
		})
		return nil
	case sortOrderingIncrementing:
		sort.Slice(*orders, func(i, j int) bool {

			return (*orders)[i].Price < (*orders)[j].Price
		})
		return nil
	default:
		return fmt.Errorf("Unknown sort order")
	}
}

func newWebsocket() (*websocket.Conn, time.Time, error) {

	ws, _, err := websocket.DefaultDialer.Dial(wsUrl(), nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	return ws, time.Now(), nil
}
