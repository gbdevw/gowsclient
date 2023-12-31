// Package which contains a WebsocketConnectionAdapterInterface implementation for
// gorilla/websocket library (https://github.com/gorilla/websocket).
package gorilla

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	wsconnadapter "github.com/gbdevw/gowse/wscengine/wsadapters"
	"github.com/gorilla/websocket"
)

// Adapter for gorilla/websocket library
type GorillaWebsocketConnectionAdapter struct {
	// Undelrying websocket connection
	conn *websocket.Conn
	// Dial options to use when opening a connection
	dialer *websocket.Dialer
	// Headers to use when opening a connection
	requestHeader http.Header
	// Internal mutex
	mu sync.Mutex
	// Internal channel of channels used to manage ping/pong
	//
	// The channel that is sent is used to wait for pong or an error.
	pingRequests chan chan error
}

// # Description
//
// Factory which creates a new GorillaWebsocketConnectionAdapter.
//
// # Inputs
//
//   - dialer: Optional dialer to use when using Dial method. If nil, the default dialer
//     defined by gorilla library will be used.
//
//   - requestHeader: Headers which will be used during Dial to specify the origin (Origin),
//     subprotocols (Sec-WebSocket-Protocol) and cookies (Cookie)
//
// # Returns
//
// New GorillaWebsocketConnectionAdapter
func NewGorillaWebsocketConnectionAdapter(dialer *websocket.Dialer, requestHeader http.Header) *GorillaWebsocketConnectionAdapter {
	if dialer == nil {
		// Use default dialer if nil
		dialer = websocket.DefaultDialer
	}
	// Build and return adapter
	return &GorillaWebsocketConnectionAdapter{
		conn:          nil,
		dialer:        dialer,
		requestHeader: requestHeader,
		mu:            sync.Mutex{},
		// Use a chan with capacity so ping requests can be recorded before sending ping message.
		pingRequests: make(chan chan error, 10),
	}
}

// # Description
//
// Dial opens a connection to the websocket server and performs a WebSocket handshake.
//
// # Inputs
//
//   - ctx: Context used for tracing/timeout purpose
//   - target: Target server URL
//
// # Returns
//
// The server response to websocket handshake or an error if any.
func (adapter *GorillaWebsocketConnectionAdapter) Dial(ctx context.Context, target url.URL) (*http.Response, error) {
	select {
	case <-ctx.Done():
		// Shortcut if context is done (timeout/cancel)
		return nil, ctx.Err()
	default:
		// Lock internal mutex before accessing internal state
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		// Check whether there is already a connection set
		if adapter.conn != nil {
			// Return error in case a connection has already been set
			return nil, fmt.Errorf("a connection has already been established")
		}
		// Open websocket connection
		conn, res, err := adapter.dialer.DialContext(ctx, target.String(), adapter.requestHeader)
		if err != nil {
			// Return response and error
			return res, err
		}
		// Persist connection internally and set handlers
		adapter.conn = conn
		conn.SetCloseHandler(adapter.closeHandler)
		conn.SetPongHandler(adapter.pongHandler)
		// Return
		return res, nil
	}
}

// # Description
//
// Send a close message with the provided status code and an optional close reason and drop
// the websocket connection.
//
// # Inputs
//
//   - ctx: Context used for tracing purpose
//   - code: Status code to use in close message
//   - reason: Optional reason joined in clsoe message. Can be empty.
//
// # Returns
//
//   - nil in case of success
//   - error: server unreachable, connection already closed, ...
func (adapter *GorillaWebsocketConnectionAdapter) Close(ctx context.Context, code wsconnadapter.StatusCode, reason string) error {
	// Lock internal mutex before accessing internal state
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	// Check whether there is already a connection set
	if adapter.conn == nil {
		return fmt.Errorf("close failed because no connection is already up")
	}
	// Close connection
	err := adapter.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(int(code), reason), time.Now().Add(60*time.Second))
	// Propagate close error to all pending Ping - This has to be done be close handler works only
	// when conneciton is closed by server (not from client side)
	propagateToAllActiveListener(adapter.pingRequests, wsconnadapter.WebsocketCloseError{
		Code:   code,
		Reason: reason,
		Err:    fmt.Errorf("client closed the connection"),
	})
	// Void connection in any case
	adapter.conn = nil
	// Return result
	return err
}

// # Description
//
// Send a ping message to the websocket server and block until a pong response is received, the
// connection is closed, or the provided context is cancelled.
//
// A separate goroutine must continuously call the Read method to process messages from the server
// so that pong responses from the server can be processed.
//
// # Inputs
//
//   - ctx: context used for tracing/timeout purpose.
//
// # Returns
//
// - nil in case of success: A Ping message has been sent to the server and a Pong has been received.
// - error: connection is closed, context timeout/cancellation, ...
func (adapter *GorillaWebsocketConnectionAdapter) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		// Shortcut if context is done (timeout/cancel)
		return ctx.Err()
	default:
		// Lock internal mutex before and store current conn reference in local variable to allow
		// other routines to perform other operations on the connection.
		adapter.mu.Lock()
		conn := adapter.conn
		adapter.mu.Unlock()
		// Check whether there is already a connection set
		if conn == nil {
			return fmt.Errorf("ping failed because no connection is already up")
		}
		// Create channel to receive pong and send it on pingRequest channel
		// It is OK because pingRequest is a channel with capacity
		// pong channel must be a blocking channel
		pong := make(chan error)
		select {
		case adapter.pingRequests <- pong:
			// Do nothing
		case <-ctx.Done():
			// Handle cancellation in case pingRequest channel is full
			return ctx.Err()
		}
		// Send Ping
		err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(60*time.Second))
		if err != nil {
			return fmt.Errorf("ping failed: %w", err)
		}
		// Wait for a pong or for ctx cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-pong:
			// Return received notification (nil or error if ping/pong failed)
			return err
		}
	}
}

// # Description
//
// Read a single message from the websocket server. Read blocks until a message is received
// from the server, or until connection is closed.
//
// Read will handle control frames from the server until a message is received:
//   - Ping from server are discarded.
//   - Close will result in a wsconnadapter.WebsocketCloseError for Read and all pending Ping.
//   - Each pong message will be used to unlock one pending Ping call.
//
// # Inputs
//
//   - ctx: Context used for tracing purpose
//
// # Returns
//
//   - MessageType: received message type (Binary | Text)
//   - []bytes: Message content
//   - error: in case of connection closure, context timeout/cancellation or failure.
func (adapter *GorillaWebsocketConnectionAdapter) Read(ctx context.Context) (wsconnadapter.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		// Shortcut if context is done (timeout/cancel)
		return -1, nil, ctx.Err()
	default:
		// Lock internal mutex before and store current conn reference in local variable to allow
		// other routines to perform other operations on the connection.
		adapter.mu.Lock()
		conn := adapter.conn
		adapter.mu.Unlock()
		// Check whether there is already a connection set
		if conn == nil {
			return -1, nil, fmt.Errorf("read failed because no connection is already up")
		}
		// Read message
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			// Check if close error
			if ce, ok := err.(*websocket.CloseError); ok {
				// Drop the existing connection so a new one can be established
				adapter.conn = nil
				// Connection is closed
				closeErr := wsconnadapter.WebsocketCloseError{
					Code:   wsconnadapter.StatusCode(ce.Code),
					Reason: err.Error(),
					Err:    err,
				}
				// Return error
				return -1, nil, closeErr
			}
			// Other errors
			return -1, nil, err
		}
		// Return message
		return wsconnadapter.MessageType(msgType), msg, nil
	}

}

// # Description
//
// Write a single message to the websocket server. Write blocks until message is sent to the
// server or until an error occurs: context timeout, cancellation, connection closed, ....
//
// # Inputs
//
//   - ctx: Context used for tracing/timeout purpose
//   - MessageType: received message type (Binary | Text)
//   - []bytes: Message content
//
// # Returns
//
//   - error: in case of connection closure, context timeout/cancellation or failure.
func (adapter *GorillaWebsocketConnectionAdapter) Write(ctx context.Context, msgType wsconnadapter.MessageType, msg []byte) error {
	select {
	case <-ctx.Done():
		// Shortcut if context is done (timeout/cancel)
		return ctx.Err()
	default:
		// Lock internal mutex as WriteMessage cannot be called concurrently
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		// Check whether there is already a connection set
		if adapter.conn == nil {
			return fmt.Errorf("write failed because no connection is already up")
		}
		// Call Write and return results
		return adapter.conn.WriteMessage(int(msgType), msg)
	}
}

// # Description
//
// Return the underlying websocket connection if any. Returned value has to be type asserted.
//
// # Returns
//
// The underlying websocket connection if any. Returned value has to be type asserted.
func (adapter *GorillaWebsocketConnectionAdapter) GetUnderlyingWebsocketConnection() any {
	// Lock internal mutex before accessing internal state
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	// Return underlying connection
	return adapter.conn
}

/*************************************************************************************************/
/* INTERNAL                                                                                      */
/*************************************************************************************************/

// Handler for received Pong which will propagate a pong notification to the first active listner
// waiting for a Pong notification.
func (adapter *GorillaWebsocketConnectionAdapter) pongHandler(appData string) error {
	// Propagate pong to first active listener
	propagateToFirstActiveListener(adapter.pingRequests, nil)
	return nil
}

// Handler for received Close which will propagate a close error notification to all active
// listeners waiting for a Pong notification.
func (adapter *GorillaWebsocketConnectionAdapter) closeHandler(code int, text string) error {
	// Build a close error and propagate it to alla ctive listeners wiaiting for a Pong.
	propagateToAllActiveListener(adapter.pingRequests, wsconnadapter.WebsocketCloseError{
		Code:   wsconnadapter.StatusCode(code),
		Reason: text,
		Err:    fmt.Errorf("close message received from server"),
	})
	return nil
}

/*************************************************************************************************/
/* UTILS                                                                                         */
/*************************************************************************************************/

// Propagate a notification to the first writeable (non-blocking write) channel received.
//
// The function returns false if the notification could not be propagated: either because no channel
// was received or because all received channels were not writeable.
func propagateToFirstActiveListener(listeners chan chan error, notification error) bool {
	for {
		select {
		case listener := <-listeners:
			// We have received a channel from a listener
			select {
			case listener <- notification:
				// Listener was active (or channel has capacity) - Notification has been sent
				return true
			default:
				// Listener was not actively listenning (not writeable) - Loop to try the next one
				continue
			}
		default:
			// No channel available to notify active listeners - Exit (false)
			return false
		}
	}
}

// Propagate a notification to all writeable (non-blocking write) channel received through
// the provided channel.
func propagateToAllActiveListener(listeners chan chan error, notification error) {
	for {
		select {
		case listener := <-listeners:
			// We have received a channel from a listener
			select {
			case listener <- notification:
				// Listener was active (or channel has capacity) - Notification has been sent
				return
			default:
				// Listener is not actively listening - Skip
				continue
			}
		default:
			// No active listeners left - Exit
			return
		}
	}
}
