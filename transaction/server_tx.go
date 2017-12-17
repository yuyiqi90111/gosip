package transaction

import (
	"fmt"
	"time"

	"github.com/discoviking/fsm"
	"github.com/ghettovoice/gosip/core"
	"github.com/ghettovoice/gosip/timing"
	"github.com/ghettovoice/gosip/transport"
)

type ServerTx interface {
	Tx
	Respond(res core.Response) error
}

type serverTx struct {
	commonTx
	timer_g      timing.Timer
	timer_g_time time.Duration
	timer_h      timing.Timer
	timer_i      timing.Timer
	timer_i_time time.Duration
	timer_j      timing.Timer
	reliable     bool
}

func NewServerTx(
	origin core.Request,
	dest string,
	tpl transport.Layer,
	msgs chan<- *IncomingMessage,
	errs chan<- error,
	cancel <-chan struct{},
) (ServerTx, error) {
	key, err := makeServerTxKey(origin)
	if err != nil {
		return nil, err
	}

	tx := new(serverTx)
	tx.key = key
	tx.origin = origin
	tx.dest = dest
	tx.tpl = tpl
	tx.msgs = msgs
	tx.errs = errs
	tx.cancel = cancel
	if viaHop, ok := tx.Origin().ViaHop(); ok {
		tx.reliable = tx.tpl.IsReliable(viaHop.Transport)
	}
	if tx.reliable {
		tx.timer_g_time = Timer_G
		tx.timer_i_time = Timer_I
	} else {
		tx.timer_i_time = 0
	}
	tx.initFSM()

	return tx, nil
}

func (tx *serverTx) String() string {
	return fmt.Sprintf("Server%s", tx.commonTx.String())
}

func (tx *serverTx) Receive(msg *transport.IncomingMessage) error {
	req, ok := msg.Msg.(core.Request)
	if !ok {
		return &core.UnexpectedMessageError{
			fmt.Errorf("%s recevied unexpected %s", tx, msg),
			req.String(),
		}
	}

	var input = fsm.NO_INPUT
	switch {
	case req == tx.Origin():
		// initial receive
		tx.msgs <- &IncomingMessage{msg, tx}
		// RFC 3261 - 17.2.1
		if req.IsInvite() {
			// todo set as timer, reset in Respond
			time.AfterFunc(200*time.Millisecond, func() {
				tx.Respond(core.NewResponseFromRequest(req, 100, "Trying", ""))
			})
		}
	case req.Method() == tx.Origin().Method():
		input = server_input_request
	case req.IsAck(): // ACK for non-2xx response
		input = server_input_ack
	default:
		return &core.UnexpectedMessageError{
			fmt.Errorf("invalid %s correlated to %s", msg, tx),
			req.String(),
		}
	}

	return tx.fsm.Spin(input)
}

func (tx *serverTx) Respond(res core.Response) error {
	tx.lastResp = res

	var input fsm.Input
	switch {
	case res.IsProvisional():
		input = server_input_user_1xx
	case res.IsSuccess():
		input = server_input_user_2xx
	default:
		input = server_input_user_300_plus
	}

	return tx.fsm.Spin(input)
}

// FSM States
const (
	server_state_trying = iota
	server_state_proceeding
	server_state_completed
	server_state_confirmed
	server_state_terminated
)

// FSM Inputs
const (
	server_input_request fsm.Input = iota
	server_input_ack
	server_input_user_1xx
	server_input_user_2xx
	server_input_user_300_plus
	server_input_timer_g
	server_input_timer_h
	server_input_timer_i
	server_input_timer_j
	server_input_transport_err
	server_input_delete
)

// Choose the right FSM init function depending on request method.
func (tx *serverTx) initFSM() {
	if tx.Origin().IsInvite() {
		tx.initInviteFSM()
	} else {
		tx.initNonInviteFSM()
	}
}

func (tx *serverTx) initInviteFSM() {
	// Define States
	tx.Log().Debugf("%s initialises INVITE FSM", tx)

	// Proceeding
	server_state_def_proceeding := fsm.State{
		Index: server_state_proceeding,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_proceeding, tx.act_respond},
			server_input_user_1xx:      {server_state_proceeding, tx.act_respond},
			server_input_user_2xx:      {server_state_terminated, tx.act_respond_delete},
			server_input_user_300_plus: {server_state_completed, tx.act_respond_complete},
			server_input_transport_err: {server_state_terminated, tx.act_trans_err},
		},
	}

	// Completed
	server_state_def_completed := fsm.State{
		Index: server_state_completed,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_completed, tx.act_respond},
			server_input_ack:           {server_state_confirmed, tx.act_confirm},
			server_input_user_1xx:      {server_state_completed, fsm.NO_ACTION},
			server_input_user_2xx:      {server_state_completed, fsm.NO_ACTION},
			server_input_user_300_plus: {server_state_completed, fsm.NO_ACTION},
			server_input_timer_g:       {server_state_completed, tx.act_respond_complete},
			server_input_timer_h:       {server_state_terminated, tx.act_timeout},
			server_input_transport_err: {server_state_terminated, tx.act_trans_err},
		},
	}

	// Confirmed
	server_state_def_confirmed := fsm.State{
		Index: server_state_confirmed,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_confirmed, fsm.NO_ACTION},
			server_input_user_1xx:      {server_state_confirmed, fsm.NO_ACTION},
			server_input_user_2xx:      {server_state_confirmed, fsm.NO_ACTION},
			server_input_user_300_plus: {server_state_confirmed, fsm.NO_ACTION},
			server_input_timer_i:       {server_state_terminated, tx.act_delete},
			//server_input_timer_g:       {server_state_confirmed, fsm.NO_ACTION},
		},
	}

	// Terminated
	server_state_def_terminated := fsm.State{
		Index: server_state_terminated,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_terminated, fsm.NO_ACTION},
			server_input_ack:           {server_state_terminated, fsm.NO_ACTION},
			server_input_user_1xx:      {server_state_terminated, fsm.NO_ACTION},
			server_input_user_2xx:      {server_state_terminated, fsm.NO_ACTION},
			server_input_user_300_plus: {server_state_terminated, fsm.NO_ACTION},
			server_input_delete:        {server_state_terminated, tx.act_delete},
		},
	}

	// Define FSM
	fsm_, err := fsm.Define(
		server_state_def_proceeding,
		server_state_def_completed,
		server_state_def_confirmed,
		server_state_def_terminated,
	)
	if err != nil {
		tx.Log().Errorf("%s failed to define FSM: will be dropped, error: %s", tx, err.Error())
		return
	}

	tx.fsm = fsm_
}

func (tx *serverTx) initNonInviteFSM() {
	// Define States
	tx.Log().Debugf("%s initialises non-INVITE FSM", tx)

	// Trying
	server_state_def_trying := fsm.State{
		Index: server_state_trying,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_trying, fsm.NO_ACTION},
			server_input_user_1xx:      {server_state_proceeding, tx.act_respond},
			server_input_user_2xx:      {server_state_completed, tx.act_respond},
			server_input_user_300_plus: {server_state_completed, tx.act_respond},
		},
	}

	// Proceeding
	server_state_def_proceeding := fsm.State{
		Index: server_state_proceeding,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_proceeding, tx.act_respond},
			server_input_user_1xx:      {server_state_proceeding, tx.act_respond},
			server_input_user_2xx:      {server_state_completed, tx.act_final},
			server_input_user_300_plus: {server_state_completed, tx.act_final},
			server_input_transport_err: {server_state_terminated, tx.act_trans_err},
		},
	}

	// Completed
	server_state_def_completed := fsm.State{
		Index: server_state_completed,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_completed, tx.act_respond},
			server_input_user_1xx:      {server_state_completed, fsm.NO_ACTION},
			server_input_user_2xx:      {server_state_completed, fsm.NO_ACTION},
			server_input_user_300_plus: {server_state_completed, fsm.NO_ACTION},
			server_input_timer_j:       {server_state_terminated, tx.act_timeout},
			server_input_transport_err: {server_state_terminated, tx.act_trans_err},
		},
	}

	// Terminated
	server_state_def_terminated := fsm.State{
		Index: server_state_terminated,
		Outcomes: map[fsm.Input]fsm.Outcome{
			server_input_request:       {server_state_terminated, fsm.NO_ACTION},
			server_input_user_1xx:      {server_state_terminated, fsm.NO_ACTION},
			server_input_user_2xx:      {server_state_terminated, fsm.NO_ACTION},
			server_input_user_300_plus: {server_state_terminated, fsm.NO_ACTION},
			server_input_timer_j:       {server_state_terminated, fsm.NO_ACTION},
			server_input_delete:        {server_state_terminated, tx.act_delete},
		},
	}

	// Define FSM
	fsm_, err := fsm.Define(
		server_state_def_trying,
		server_state_def_proceeding,
		server_state_def_completed,
		server_state_def_terminated,
	)
	if err != nil {
		tx.Log().Errorf("%s failed to define FSM: will be dropped, error: %s", tx, err.Error())
		return
	}

	tx.fsm = fsm_
}

// Define actions.
// Send response
func (tx *serverTx) act_respond() fsm.Input {
	tx.lastErr = tx.tpl.Send(tx.Destination(), tx.lastResp)
	if tx.lastErr != nil {
		return server_input_transport_err
	}

	return fsm.NO_INPUT
}

func (tx *serverTx) act_respond_complete() fsm.Input {
	tx.lastErr = tx.tpl.Send(tx.Destination(), tx.lastResp)
	if tx.lastErr != nil {
		return server_input_transport_err
	}

	if !tx.reliable {
		if tx.timer_g == nil {
			tx.timer_g = timing.AfterFunc(tx.timer_g_time, func() {
				tx.Log().Debugf("%s, timer_g fired", tx)
				tx.fsm.Spin(server_input_timer_g)
			})
		} else {
			tx.timer_g_time *= 2
			if tx.timer_g_time > T2 {
				tx.timer_g_time = T2
			}
			tx.timer_g.Reset(tx.timer_g_time)
		}
	}
	if tx.timer_h == nil {
		tx.timer_h = timing.AfterFunc(Timer_H, func() {
			tx.Log().Debugf("%s, timer_h fired", tx)
			tx.fsm.Spin(server_input_timer_h)
		})
	}

	return fsm.NO_INPUT
}

// Send final response
func (tx *serverTx) act_final() fsm.Input {
	tx.lastErr = tx.tpl.Send(tx.Destination(), tx.lastResp)
	if tx.lastErr != nil {
		return server_input_transport_err
	}

	tx.timer_j = timing.AfterFunc(Timer_J, func() {
		tx.Log().Debugf("%s, timer_j fired")
		tx.fsm.Spin(server_input_timer_j)
	})

	return fsm.NO_INPUT
}

// Inform user of transport error
func (tx *serverTx) act_trans_err() fsm.Input {
	tx.errs <- &TxTransportError{
		fmt.Errorf("%s failed to send %s: %s", tx, tx.lastResp, tx.lastErr),
		tx.Key(),
		tx.String(),
	}
	return server_input_delete
}

// Inform user of timeout error
func (tx *serverTx) act_timeout() fsm.Input {
	tx.errs <- &TxTimeoutError{
		fmt.Errorf("%s timed out", tx),
		tx.Key(),
		tx.String(),
	}
	return server_input_delete
}

// Just delete the transaction.
func (tx *serverTx) act_delete() fsm.Input {
	tx.errs <- &TxTerminatedError{
		fmt.Errorf("%s terminated", tx),
		tx.Key(),
		tx.String(),
	}
	return fsm.NO_INPUT
}

// Send response and delete the transaction.
func (tx *serverTx) act_respond_delete() fsm.Input {
	tx.errs <- &TxTerminatedError{
		fmt.Errorf("%s terminated", tx),
		tx.Key(),
		tx.String(),
	}

	tx.lastErr = tx.tpl.Send(tx.dest, tx.lastResp)
	if tx.lastErr != nil {
		return server_input_transport_err
	}

	return fsm.NO_INPUT
}

func (tx *serverTx) act_confirm() fsm.Input {
	tx.timer_i = timing.AfterFunc(Timer_I, func() {
		tx.Log().Debugf("%s, timer_i fired")
		tx.fsm.Spin(server_input_timer_i)
	})
	return fsm.NO_INPUT
}