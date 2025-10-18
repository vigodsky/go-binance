package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/common/websocket"
	"github.com/adshao/go-binance/v2/futures"
)

var (
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
)

const requestsCount = 100

// Stats tracks request/response statistics
type Stats struct {
	RequestsSent      int
	ResponsesReceived int
	StartTime         time.Time
	EndTime           time.Time
	mu                sync.Mutex
}

func (s *Stats) IncrementRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RequestsSent++
}

func (s *Stats) IncrementResponses() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ResponsesReceived++
}

func (s *Stats) GetCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.RequestsSent, s.ResponsesReceived
}

func (s *Stats) SetStartTime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.StartTime = time.Now()
}

func (s *Stats) SetEndTime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EndTime = time.Now()
}

func (s *Stats) GetDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

var (
	placeStats  = &Stats{}
	cancelStats = &Stats{}
)

// MultiplexedFuturesWebSocketExample demonstrates concurrent WebSocket requests
// with waiter channels for 1:1 request/response matching
func MultiplexedFuturesWebSocketExample() {
	// Setup structured logging
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("=== Enhanced Multiplexed Futures WebSocket Example ===")
	logger.Info("Testing concurrent requests with UUID tracking and waiter channels")

	// Validate configuration
	if err := AppConfig.Validate(); err != nil {
		logger.Error("Configuration error", "error", err)
		return
	}

	// Setup testnet and context
	AppConfig.SetupTestnet()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	// Create shared WebSocket connection and client
	sharedClient, err := createWebSocketClient()
	if err != nil {
		logger.Error("Failed to create WebSocket client", "error", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2) // 2 services will run

	// Start monitoring and services
	go startFallbackMonitoring(sharedClient)
	go startOrderPlaceService(&wg, sharedClient)
	go startOrderCancelService(&wg, sharedClient)

	wg.Wait()
	logger.Info("All services completed")

	// Print final statistics
	printFinalStats()
}

// createWebSocketClient creates a new WebSocket client
func createWebSocketClient() (websocket.Client, error) {
	conn, err := websocket.NewConnection(
		futures.WsApiInitReadWriteConn,
		futures.WebsocketKeepalive,
		futures.WebsocketTimeoutReadWriteConnection,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create WebSocket connection: %w", err)
	}

	client, err := websocket.NewClient(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to create WebSocket client: %w", err)
	}

	logger.Info("Shared futures WebSocket client created successfully")
	return client, nil
}

// startFallbackMonitoring monitors the shared channel for fallback messages
func startFallbackMonitoring(sharedClient websocket.Client) {
	logger.Info("Starting fallback message monitoring")

	for {
		select {
		case data := <-sharedClient.GetReadChannel():
			logger.Error("FALLBACK: Message received on shared channel (should be routed to waiter)",
				"data", string(data))
		case err := <-sharedClient.GetReadErrorChannel():
			if err != nil {
				logger.Error("Shared channel error", "error", err)
				return
			}
		case <-ctx.Done():
			logger.Info("Fallback monitoring stopped")
			return
		}
	}
}

// startOrderPlaceService handles order placement requests
func startOrderPlaceService(wg *sync.WaitGroup, sharedClient websocket.Client) {
	defer wg.Done()

	// Create order place service
	orderPlaceService, err := futures.NewOrderPlaceWsService(
		AppConfig.APIKey,
		AppConfig.SecretKey,
		websocket.WithWebSocketClient(sharedClient),
	)
	if err != nil {
		logger.Error("Failed to create order place service", "error", err)
		return
	}

	// Create dedicated waiter channel
	waiterChannel := make(chan []byte, 1)
	defer close(waiterChannel)

	// Initialize stats
	placeStats.SetStartTime()

	// Send multiple requests with concurrency control
	go sendOrderPlaceRequests(orderPlaceService, waiterChannel)

	// Start response listener
	handleOrderPlaceResponses(waiterChannel)
}

// handleOrderPlaceResponses handles responses from order place service
func handleOrderPlaceResponses(waiterChannel <-chan []byte) {
	serviceLogger := logger.With("service", "order_place")
	timeout := time.NewTimer(30 * time.Second) // Overall timeout for all responses
	defer timeout.Stop()

	for {
		select {
		case data, ok := <-waiterChannel:
			if !ok {
				serviceLogger.Info("WaiterChannel closed")
				placeStats.SetEndTime()
				return
			}
			response := string(data)
			serviceLogger.Debug("Response received", "response", response)

			if !strings.Contains(response, "place_") {
				serviceLogger.Error("Response does not contain expected prefix 'place_'", "response", response)
				placeStats.SetEndTime()
				return
			}

			placeStats.IncrementResponses()
			sent, received := placeStats.GetCounts()
			serviceLogger.Info("Progress", "responses_received", received, "requests_sent", sent)

			if received >= requestsCount {
				serviceLogger.Info("All responses received successfully", "total", received)
				placeStats.SetEndTime()
				return
			}

		case <-timeout.C:
			sent, received := placeStats.GetCounts()
			serviceLogger.Warn("Overall timeout reached",
				"requests_sent", sent,
				"responses_received", received,
				"missing", sent-received)
			placeStats.SetEndTime()
			return

		case <-ctx.Done():
			serviceLogger.Info("Response handler stopped")
			placeStats.SetEndTime()
			return
		}
	}
}

// sendOrderPlaceRequests sends multiple order placement requests concurrently
func sendOrderPlaceRequests(orderPlaceService *futures.OrderPlaceWsService, waiterChannel chan []byte) {
	serviceLogger := logger.With("service", "order_place")

	// Build request
	request := futures.NewOrderPlaceWsRequest().
		Symbol("BTCUSDT").
		Side(futures.SideTypeBuy).
		Type(futures.OrderTypeMarket).
		TimeInForce(futures.TimeInForceTypeFOK).
		Quantity("0.00001")

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Limit concurrent requests

	for i := 0; i < requestsCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}        // Acquire semaphore
			defer func() { <-semaphore }() // Release semaphore

			requestID := fmt.Sprintf("place_%d_%d", time.Now().UnixNano(), index)
			serviceLogger.Debug("Sending request", "request_id", requestID, "index", index)

			if err := orderPlaceService.Do(requestID, request, websocket.WithWaiter(waiterChannel)); err != nil {
				serviceLogger.Error("Failed to send request", "error", err, "request_id", requestID)
				return
			}
			placeStats.IncrementRequests()
		}(i)

		// Rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	wg.Wait()
	serviceLogger.Info("All order place requests sent")
}

// startOrderCancelService handles order cancellation requests
func startOrderCancelService(wg *sync.WaitGroup, sharedClient websocket.Client) {
	defer wg.Done()

	// Create order cancel service
	orderCancelService, err := futures.NewOrderCancelWsService(
		AppConfig.APIKey,
		AppConfig.SecretKey,
		websocket.WithWebSocketClient(sharedClient),
	)
	if err != nil {
		logger.Error("Failed to create order cancel service", "error", err)
		return
	}

	// Create dedicated waiter channel
	waiterChannel := make(chan []byte, 1)
	defer close(waiterChannel)

	// Initialize stats
	cancelStats.SetStartTime()

	// Send multiple requests with concurrency control
	go sendOrderCancelRequests(orderCancelService, waiterChannel)

	// Start response listener
	handleOrderCancelResponses(waiterChannel)
}

// handleOrderCancelResponses handles responses from order cancel service
func handleOrderCancelResponses(waiterChannel <-chan []byte) {
	serviceLogger := logger.With("service", "order_cancel")
	timeout := time.NewTimer(30 * time.Second) // Overall timeout for all responses
	defer timeout.Stop()

	for {
		select {
		case data, ok := <-waiterChannel:
			if !ok {
				serviceLogger.Info("WaiterChannel closed")
				cancelStats.SetEndTime()
				return
			}
			response := string(data)
			serviceLogger.Debug("Response received", "response", response)

			if !strings.Contains(response, "cancel_") {
				serviceLogger.Error("Response does not contain expected prefix 'cancel_'", "response", response)
				cancelStats.SetEndTime()
				return
			}

			cancelStats.IncrementResponses()
			sent, received := cancelStats.GetCounts()
			serviceLogger.Info("Progress", "responses_received", received, "requests_sent", sent)

			if received >= requestsCount {
				serviceLogger.Info("All responses received successfully", "total", received)
				cancelStats.SetEndTime()
				return
			}

		case <-timeout.C:
			sent, received := cancelStats.GetCounts()
			serviceLogger.Warn("Overall timeout reached",
				"requests_sent", sent,
				"responses_received", received,
				"missing", sent-received)
			cancelStats.SetEndTime()
			return

		case <-ctx.Done():
			serviceLogger.Info("Response handler stopped")
			cancelStats.SetEndTime()
			return
		}
	}
}

// sendOrderCancelRequests sends multiple order cancellation requests concurrently
func sendOrderCancelRequests(orderCancelService *futures.OrderCancelWsService, waiterChannel chan []byte) {
	serviceLogger := logger.With("service", "order_cancel")

	// Build request
	request := futures.NewOrderCancelRequest().
		Symbol("BTCUSDT").
		OrderID(123123) // Non-existing order for testing

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Limit concurrent requests

	for i := 0; i < requestsCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}        // Acquire semaphore
			defer func() { <-semaphore }() // Release semaphore

			requestID := fmt.Sprintf("cancel_%d_%d", time.Now().UnixNano(), index)
			serviceLogger.Debug("Sending request", "request_id", requestID, "index", index)

			if err := orderCancelService.Do(requestID, request, websocket.WithWaiter(waiterChannel)); err != nil {
				serviceLogger.Error("Failed to send request", "error", err, "request_id", requestID)
				return
			}
			cancelStats.IncrementRequests()
		}(i)

		// Rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	wg.Wait()
	serviceLogger.Info("All order cancel requests sent")
}

// printFinalStats prints comprehensive statistics for both services
func printFinalStats() {
	logger.Info("=== FINAL STATISTICS ===")

	// Place stats
	placeSent, placeReceived := placeStats.GetCounts()
	placeDuration := placeStats.GetDuration()
	placeSuccess := placeSent == placeReceived && placeSent == requestsCount

	logger.Info("Order Place Service Statistics",
		"requests_sent", placeSent,
		"responses_received", placeReceived,
		"expected", requestsCount,
		"missing", placeSent-placeReceived,
		"success", placeSuccess,
		"duration", placeDuration,
		"avg_response_time", placeDuration/time.Duration(max(1, placeReceived)),
	)

	// Cancel stats
	cancelSent, cancelReceived := cancelStats.GetCounts()
	cancelDuration := cancelStats.GetDuration()
	cancelSuccess := cancelSent == cancelReceived && cancelSent == requestsCount

	logger.Info("Order Cancel Service Statistics",
		"requests_sent", cancelSent,
		"responses_received", cancelReceived,
		"expected", requestsCount,
		"missing", cancelSent-cancelReceived,
		"success", cancelSuccess,
		"duration", cancelDuration,
		"avg_response_time", cancelDuration/time.Duration(max(1, cancelReceived)),
	)

	// Overall stats
	totalSent := placeSent + cancelSent
	totalReceived := placeReceived + cancelReceived
	totalExpected := requestsCount * 2
	overallSuccess := totalSent == totalReceived && totalSent == totalExpected

	logger.Info("Overall Statistics",
		"total_requests_sent", totalSent,
		"total_responses_received", totalReceived,
		"total_expected", totalExpected,
		"total_missing", totalSent-totalReceived,
		"overall_success", overallSuccess,
		"success_rate", fmt.Sprintf("%.2f%%", float64(totalReceived)/float64(totalSent)*100),
	)

	if !overallSuccess {
		logger.Error("VERIFICATION FAILED: Request/Response counts do not match!",
			"sent", totalSent,
			"received", totalReceived,
			"expected", totalExpected,
		)
	} else {
		logger.Info("✅ VERIFICATION PASSED: All requests received responses!")
	}
}
