package factory

import (
	"fmt"

	"github.com/thecodedproject/crypto"
	"github.com/thecodedproject/crypto/exchangesdk"
	"github.com/thecodedproject/crypto/exchangesdk/binance"
	"github.com/thecodedproject/crypto/exchangesdk/dummyclient"
	"github.com/thecodedproject/crypto/exchangesdk/luno"
)

func NewClient(
	exchange crypto.Exchange,
	apiKey string,
	apiSecret string,
) (exchangesdk.Client, error) {

	switch exchange.Provider {
	case crypto.ApiProviderLuno:
		return luno.NewClient(
			apiKey,
			apiSecret,
			exchange.Pair,
		)
	case crypto.ApiProviderBinance:
		return binance.NewClient(
			apiKey,
			apiSecret,
			exchange.Pair,
		)
	case crypto.ApiProviderDummyExchange:
		return dummyclient.NewClient(
			apiKey,
			apiSecret,
			exchange.Pair,
		)
	default:
		return nil, fmt.Errorf("Cannot create client; Unknown Api provider %s", exchange.Provider)
	}

}
