package stp

import (
	"context"
	"errors"
	"sync"
)

// OrderState is STP's view of an outbound order (consulta de orden), reduced
// to what payout recovery needs.
type OrderState string

const (
	// OrderLiquidated: the SPEI transfer was settled to the beneficiary.
	OrderLiquidated OrderState = "LIQUIDATED"
	// OrderAccepted: STP registered the order; liquidation still in progress.
	OrderAccepted OrderState = "ACCEPTED"
	// OrderRejected: cancelled, rejected or returned (devuelta); no money left.
	OrderRejected OrderState = "REJECTED"
	// OrderNotFound: STP has no order with that clave de rastreo.
	OrderNotFound OrderState = "NOT_FOUND"
	// OrderUnknown: STP answered without a usable state.
	OrderUnknown OrderState = "UNKNOWN"
)

// OrderStatus is the result of an order status query.
type OrderStatus struct {
	State      OrderState
	STPOrderID int64
	Detail     string
}

// ErrOrderStatusUnavailable is returned when the status cannot be obtained
// (network failure, STP outage); the caller must treat the outcome as unknown.
var ErrOrderStatusUnavailable = errors.New("STP order status unavailable")

// OrderStatusClient queries the status of an outbound order by clave de
// rastreo and operation date (YYYYMMDD). The real STP consulta endpoint is
// wired with the STP contract (Phase 2); MockSTPClient and
// MockOrderStatusClient implement it for dev and tests.
type OrderStatusClient interface {
	QueryOrderStatus(ctx context.Context, claveRastreo, fechaOperacion string) (OrderStatus, error)
}

var _ OrderStatusClient = (*MockSTPClient)(nil)

// QueryOrderStatus reports orders this mock accepted as liquidated and any
// other clave as not found.
func (m *MockSTPClient) QueryOrderStatus(ctx context.Context, claveRastreo, _ string) (OrderStatus, error) {
	if err := ctx.Err(); err != nil {
		return OrderStatus{}, errors.Join(ErrOrderStatusUnavailable, err)
	}
	if v, ok := m.orders.Load(claveRastreo); ok {
		return OrderStatus{State: OrderLiquidated, STPOrderID: v.(int64)}, nil
	}
	return OrderStatus{State: OrderNotFound}, nil
}

// MockOrderStatusClient returns scripted statuses (tests).
type MockOrderStatusClient struct {
	mu       sync.Mutex
	statuses map[string]OrderStatus
	errs     map[string]error
	calls    map[string]int
}

func NewMockOrderStatusClient() *MockOrderStatusClient {
	return &MockOrderStatusClient{statuses: map[string]OrderStatus{}, errs: map[string]error{}, calls: map[string]int{}}
}

// Set scripts the status returned for claveRastreo.
func (m *MockOrderStatusClient) Set(claveRastreo string, st OrderStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[claveRastreo] = st
	delete(m.errs, claveRastreo)
}

// SetError makes queries for claveRastreo fail with err.
func (m *MockOrderStatusClient) SetError(claveRastreo string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs[claveRastreo] = err
}

// Calls returns how many times claveRastreo was queried.
func (m *MockOrderStatusClient) Calls(claveRastreo string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[claveRastreo]
}

func (m *MockOrderStatusClient) QueryOrderStatus(_ context.Context, claveRastreo, _ string) (OrderStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[claveRastreo]++
	if err, ok := m.errs[claveRastreo]; ok {
		return OrderStatus{}, err
	}
	if st, ok := m.statuses[claveRastreo]; ok {
		return st, nil
	}
	return OrderStatus{State: OrderUnknown}, nil
}
