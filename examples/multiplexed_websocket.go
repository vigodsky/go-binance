package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/common/websocket"
	"github.com/adshao/go-binance/v2/futures"
)

// ResponseCounter tracks request/response counts
type ResponseCounter struct {
	TotalRequests   int64
	TotalResponses  int64
	PlaceRequests   int64
	PlaceResponses  int64
	StatusRequests  int64
	StatusResponses int64
	CancelRequests  int64
	CancelResponses int64
}

// RequestResponse tracks individual request/response pairs
type RequestResponse struct {
	UUID        string
	RequestType string
	Timestamp   time.Time
	Responded   bool
}

// MultiplexedFuturesWebSocketExample demonstrates concurrent WebSocket requests
// with UUID tracking and waiter channels for 1:1 request/response matching
func MultiplexedFuturesWebSocketExample() {
	log.Println("=== Enhanced Multiplexed Futures WebSocket Example ===")
	log.Println("Testing concurrent requests with UUID tracking and waiter channels")

	if err := AppConfig.Validate(); err != nil {
		log.Printf("Configuration error: %v\n", err)
		return
	}

	// Setup testnet
	AppConfig.SetupTestnet()

	// Create a shared WebSocket connection and client for futures
	conn, err := websocket.NewConnection(futures.WsApiInitReadWriteConn, futures.WebsocketKeepalive, futures.WebsocketTimeoutReadWriteConnection)
	if err != nil {
		log.Printf("Failed to create WebSocket connection: %v\n", err)
		return
	}

	sharedClient, err := websocket.NewClient(conn)
	if err != nil {
		log.Printf("Failed to create WebSocket client: %v\n", err)
		return
	}
	log.Printf("Shared futures WebSocket client created successfully\n\n")

	var wg sync.WaitGroup
	wg.Add(2) // 2 reading goroutines must complete
	go startFallbackMessages(sharedClient)
	go startOrderPlaceService(&wg, sharedClient)
	go startOrderCancelService(&wg, sharedClient)

	wg.Wait()
}

// startFallbackMessages start monitoring goroutine for fallback messages
func startFallbackMessages(sharedClient websocket.Client) {
	go func() {
		log.Println("Monitoring shared channel for fallback messages...")

		for {
			select {
			case data := <-sharedClient.GetReadChannel():
				log.Fatalf("FALLBACK #%s: Message received on shared channel (should be routed to waiter!)\n", string(data))
			case err := <-sharedClient.GetReadErrorChannel():
				if err != nil {
					log.Fatalf("Shared channel error: %v\n", err)
				}
			}
		}
	}()
}

func startOrderPlaceService(wg *sync.WaitGroup, sharedClient websocket.Client) {
	// Create services
	orderPlaceService, err := futures.NewOrderPlaceWsService(
		AppConfig.APIKey,
		AppConfig.SecretKey,
		websocket.WithWebSocketClient(sharedClient),
	)
	if err != nil {
		log.Printf("Failed to create order place service: %v\n", err)
		return
	}

	// Create dedicated waiter channel
	waiterOrderPlace := make(chan []byte, 1)

	go func() {
		defer wg.Done()

		for {
			// Wait for response with timeout
			select {
			case data := <-waiterOrderPlace:
				response := string(data)
				log.Println("order.place response:", response)

				if !strings.Contains(response, "place_") {
					// if not expected response received, exit 1
					log.Fatalln("order place response does not contain prefix <place_>, something went wrong with routing")
				}

			case <-time.After(5 * time.Second):
				log.Println("order.place timeout")
				return
			}
		}
	}()

	// Build request
	request := futures.NewOrderPlaceWsRequest().
		Symbol("BTCUSDT"). // Use valid symbol
		Side(futures.SideTypeBuy).
		Type(futures.OrderTypeMarket).
		TimeInForce(futures.TimeInForceTypeFOK).
		Quantity("0.00001") // Small quantity for testing

	const requestsCount = 100
	// Send multiple concurrent requests with single waiter
	for range requestsCount {
		requestID := fmt.Sprintf("place_%d", time.Now().UnixNano())
		go func() {
			log.Println("order.place sending request:", requestID)
			if err := orderPlaceService.Do(requestID, request, websocket.WithWaiter(waiterOrderPlace)); err != nil {
				log.Fatal(err)
			}
		}()

		time.Sleep(100 * time.Millisecond)
	}
}

func startOrderCancelService(wg *sync.WaitGroup, sharedClient websocket.Client) {
	// Create services
	orderCancelWsService, err := futures.NewOrderCancelWsService(
		AppConfig.APIKey,
		AppConfig.SecretKey,
		websocket.WithWebSocketClient(sharedClient),
	)
	if err != nil {
		log.Printf("Failed to create order cancel service: %v\n", err)
		return
	}

	// Create dedicated waiter channel
	waiterOrderCancel := make(chan []byte, 1)
	go func() {
		defer wg.Done()

		for {
			// Wait for response with timeout
			select {
			case data := <-waiterOrderCancel:
				response := string(data)
				log.Println("order.cancel response:", response)

				if !strings.Contains(response, "cancel_") {
					// if not expected response received, exit 1
					log.Fatalln("order cancel response does not contain prefix <cancel_>, something went wrong with routing")
				}

			case <-time.After(5 * time.Second):
				log.Println("order.cancel timeout")
				return
			}
		}
	}()

	// Build request
	request := futures.NewOrderCancelRequest().
		Symbol("BTCUSDT").
		OrderID(123123) // not existing order for testing

	const requestsCount = 100
	// Send multiple concurrent requests with single waiter
	for range requestsCount {
		requestID := fmt.Sprintf("cancel_%d", time.Now().UnixNano())
		go func() {
			log.Println("order.cancel sending request:", requestID)
			if err := orderCancelWsService.Do(requestID, request, websocket.WithWaiter(waiterOrderCancel)); err != nil {
				log.Fatal(err)
			}
		}()

		time.Sleep(100 * time.Millisecond)
	}
}
