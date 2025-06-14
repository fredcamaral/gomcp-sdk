package transport

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/LerianStudio/gomcp-sdk/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSocketTransport_StartStop(t *testing.T) {
	config := &WebSocketConfig{
		Address:      "localhost:0",
		Path:         "/ws",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start transport
	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	assert.True(t, transport.IsRunning())

	// Try starting again (should fail)
	err = transport.Start(ctx, handler)
	assert.Error(t, err)

	// Stop transport
	err = transport.Stop()
	require.NoError(t, err)
	assert.False(t, transport.IsRunning())
}

func TestWebSocketTransport_Connection(t *testing.T) {
	config := &WebSocketConfig{
		Address:      "localhost:0",
		Path:         "/ws",
		PingInterval: 1 * time.Second,
		PongTimeout:  2 * time.Second,
	}

	transport := NewWebSocketTransport(config)

	handler := &mockHandler{
		handleFunc: func(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
			return &protocol.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]interface{}{
					"method": req.Method,
					"params": req.Params,
				},
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	// Get actual address
	addr := transport.Address()

	// Connect WebSocket client
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Wait for connection to be registered
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, transport.ConnectionCount())

	// Send request
	req := &protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "test.method",
		Params:  map[string]interface{}{"key": "value"},
	}

	err = conn.WriteJSON(req)
	require.NoError(t, err)

	// Read response
	var resp protocol.JSONRPCResponse
	err = conn.ReadJSON(&resp)
	require.NoError(t, err)

	assert.Equal(t, "2.0", resp.JSONRPC)

	// Compare IDs handling JSON number conversion
	switch expected := req.ID.(type) {
	case int:
		if actual, ok := resp.ID.(float64); ok {
			assert.Equal(t, float64(expected), actual)
		} else {
			assert.Equal(t, req.ID, resp.ID)
		}
	case int64:
		if actual, ok := resp.ID.(float64); ok {
			assert.Equal(t, float64(expected), actual)
		} else {
			assert.Equal(t, req.ID, resp.ID)
		}
	default:
		assert.Equal(t, req.ID, resp.ID)
	}

	assert.Nil(t, resp.Error)

	result := resp.Result.(map[string]interface{})
	assert.Equal(t, "test.method", result["method"])

	// Close connection
	_ = conn.Close()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, transport.ConnectionCount())
}

func TestWebSocketTransport_MultipleConnections(t *testing.T) {
	config := &WebSocketConfig{
		Address: "localhost:0",
		Path:    "/ws",
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()

	// Connect multiple clients
	numClients := 5
	clients := make([]*websocket.Conn, numClients)

	for i := 0; i < numClients; i++ {
		u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		require.NoError(t, err)
		clients[i] = conn
	}

	// Wait for connections to be registered
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, numClients, transport.ConnectionCount())

	// Test broadcast
	testMessage := map[string]interface{}{
		"type": "broadcast",
		"data": "test",
	}

	err = transport.Broadcast(testMessage)
	require.NoError(t, err)

	// All clients should receive the broadcast
	for _, conn := range clients {
		var msg interface{}
		err = conn.ReadJSON(&msg)
		require.NoError(t, err)
		assert.Equal(t, testMessage, msg)
	}

	// Close all connections
	for _, conn := range clients {
		_ = conn.Close()
	}

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, transport.ConnectionCount())
}

func TestWebSocketTransport_ErrorHandling(t *testing.T) {
	config := &WebSocketConfig{
		Address:        "localhost:0",
		Path:           "/ws",
		MaxMessageSize: 100, // Small for testing
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()

	// Connect client
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	tests := []struct {
		name        string
		message     interface{}
		expectError bool
		errorCode   int
	}{
		{
			name:        "invalid json",
			message:     websocket.TextMessage,
			expectError: true,
			errorCode:   protocol.ParseError,
		},
		{
			name: "valid message",
			message: &protocol.JSONRPCRequest{
				JSONRPC: "2.0",
				ID:      1,
				Method:  "test",
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "invalid json" {
				// Send invalid JSON
				err = conn.WriteMessage(websocket.TextMessage, []byte("{invalid json}"))
				require.NoError(t, err)
			} else {
				// Send valid message
				err = conn.WriteJSON(tt.message)
				require.NoError(t, err)
			}

			// Read response
			var resp protocol.JSONRPCResponse
			err = conn.ReadJSON(&resp)
			require.NoError(t, err)

			if tt.expectError {
				assert.NotNil(t, resp.Error)
				assert.Equal(t, tt.errorCode, resp.Error.Code)
			} else {
				assert.Nil(t, resp.Error)
			}
		})
	}
}

func TestWebSocketTransport_Compression(t *testing.T) {
	config := &WebSocketConfig{
		Address:           "localhost:0",
		Path:              "/ws",
		EnableCompression: true,
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{
		handleFunc: func(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
			// Return large response to test compression
			largeData := make([]string, 1000)
			for i := range largeData {
				largeData[i] = "This is a test string for compression"
			}

			return &protocol.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  map[string]interface{}{"data": largeData},
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()

	// Connect with compression enabled
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	dialer := websocket.Dialer{
		EnableCompression: true,
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Send request
	req := &protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "test.compression",
	}

	err = conn.WriteJSON(req)
	require.NoError(t, err)

	// Read large response
	var resp protocol.JSONRPCResponse
	err = conn.ReadJSON(&resp)
	require.NoError(t, err)

	assert.Nil(t, resp.Error)
	assert.NotNil(t, resp.Result)
}

func TestWebSocketTransport_PingPong(t *testing.T) {
	config := &WebSocketConfig{
		Address:      "localhost:0",
		Path:         "/ws",
		PingInterval: 100 * time.Millisecond,
		PongTimeout:  2 * time.Second,
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()

	// Connect client
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Test server->client ping
	pingReceived := make(chan bool, 1)

	// Set a custom ping handler to detect server pings
	conn.SetPingHandler(func(appData string) error {
		pingReceived <- true
		// Send pong response
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	// Read messages in background
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	// Wait for server to send a ping
	select {
	case <-pingReceived:
		// Success - server is sending pings
	case <-time.After(1 * time.Second):
		t.Fatal("No ping received from server")
	}

	// Test client->server ping
	pongReceived := make(chan bool, 1)
	conn.SetPongHandler(func(string) error {
		pongReceived <- true
		return nil
	})

	// Send a ping from client
	err = conn.WriteControl(websocket.PingMessage, []byte("test"), time.Now().Add(time.Second))
	require.NoError(t, err)

	// Server should respond with pong
	select {
	case <-pongReceived:
		// Success - got pong from server
	case <-time.After(500 * time.Millisecond):
		t.Fatal("No pong received from server")
	}
}

func TestWebSocketTransport_CheckOrigin(t *testing.T) {
	allowedOrigins := map[string]bool{
		"https://example.com": true,
		"https://test.com":    true,
	}

	config := &WebSocketConfig{
		Address: "localhost:0",
		Path:    "/ws",
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return allowedOrigins[origin]
		},
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(t, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}

	tests := []struct {
		name          string
		origin        string
		shouldConnect bool
	}{
		{
			name:          "allowed origin",
			origin:        "https://example.com",
			shouldConnect: true,
		},
		{
			name:          "disallowed origin",
			origin:        "https://malicious.com",
			shouldConnect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("Origin", tt.origin)

			conn, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)

			if tt.shouldConnect {
				require.NoError(t, err)
				assert.NotNil(t, conn)
				_ = conn.Close()
			} else {
				assert.Error(t, err)
				if resp != nil {
					assert.Equal(t, http.StatusForbidden, resp.StatusCode)
				}
			}
		})
	}
}

// Benchmark tests

func BenchmarkWebSocketTransport_SingleConnection(b *testing.B) {
	config := &WebSocketConfig{
		Address: "localhost:0",
		Path:    "/ws",
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(b, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()
	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	require.NoError(b, err)
	defer func() { _ = conn.Close() }()

	req := &protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "benchmark.test",
		Params:  map[string]interface{}{"data": "test"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.ID = i
		err := conn.WriteJSON(req)
		if err != nil {
			b.Fatal(err)
		}

		var resp protocol.JSONRPCResponse
		err = conn.ReadJSON(&resp)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWebSocketTransport_Broadcast(b *testing.B) {
	config := &WebSocketConfig{
		Address: "localhost:0",
		Path:    "/ws",
	}

	transport := NewWebSocketTransport(config)
	handler := &mockHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := transport.Start(ctx, handler)
	require.NoError(b, err)
	defer func() { _ = transport.Stop() }()

	addr := transport.Address()

	// Connect multiple clients
	numClients := 10
	for i := 0; i < numClients; i++ {
		u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		require.NoError(b, err)
		defer func() { _ = conn.Close() }()

		// Read messages in background
		go func() {
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
			}
		}()
	}

	// Wait for connections
	time.Sleep(100 * time.Millisecond)

	message := map[string]interface{}{
		"type": "benchmark",
		"data": "test broadcast message",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := transport.Broadcast(message)
		if err != nil {
			b.Fatal(err)
		}
	}
}
