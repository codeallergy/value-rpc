/*
 * Copyright (c) 2023 Zander Schwid & Co. LLC.
 * SPDX-License-Identifier: BUSL-1.1
 */

package valueserver

import (
	"fmt"
	"github.com/codeallergy/value"
	 vrpc "github.com/codeallergy/value-rpc/valuerpc"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"sync"
)

var OutgoingQueueCap = 4096

type servingClient struct {
	clientId    int64
	activeConn  atomic.Value
	functionMap *sync.Map

	logger *zap.Logger

	outgoingQueue chan value.Map

	requestMap        sync.Map
	canceledRequests  sync.Map

	closeOnce sync.Once
}

func NewServingClient(clientId int64, conn vrpc.MsgConn, functionMap *sync.Map, logger *zap.Logger) *servingClient {

	client := &servingClient{
		clientId:      clientId,
		functionMap:   functionMap,
		outgoingQueue: make(chan value.Map, OutgoingQueueCap),
		logger:        logger,
	}
	client.activeConn.Store(conn)

	go client.sender()

	return client
}

func (t *servingClient) Close() {

	t.closeOnce.Do(func() {
		t.requestMap.Range(func(key, value interface{}) bool {
			sr := value.(*servingRequest)
			sr.Close()
			return true
		})

		close(t.outgoingQueue)
	})

}

func (t *servingClient) replaceConn(newConn vrpc.MsgConn) {

	oldConn := t.activeConn.Load()
	if oldConn != nil {
		oldConn.(vrpc.MsgConn).Close()
	}

	t.activeConn.Store(newConn)
	go t.sender()
}

func FunctionResult(requestId value.Number, result value.Value) value.Map {
	resp := value.EmptyMap().
		Put(vrpc.MessageTypeField, vrpc.FunctionResponse.Long()).
		Put(vrpc.RequestIdField, requestId)
	if result != nil {
		return resp.Put(vrpc.ResultField, result)
	} else {
		return resp
	}
}

func StreamReady(requestId value.Number) value.Map {
	return value.EmptyMap().
		Put(vrpc.MessageTypeField, vrpc.StreamReady.Long()).
		Put(vrpc.RequestIdField, requestId)
}

func StreamValue(requestId value.Number, val value.Value) value.Map {
	return value.EmptyMap().
		Put(vrpc.MessageTypeField, vrpc.StreamValue.Long()).
		Put(vrpc.RequestIdField, requestId).
		Put(vrpc.ValueField, val)
}

func StreamEnd(requestId value.Number, val value.Value) value.Map {
	resp := value.EmptyMap().
		Put(vrpc.MessageTypeField, vrpc.StreamEnd.Long()).
		Put(vrpc.RequestIdField, requestId)
	if val != nil {
		return resp.Put(vrpc.ValueField, val)
	} else {
		return resp
	}
}

func FunctionError(requestId value.Number, format string, args ...interface{}) value.Map {
	resp := value.EmptyMap().
		Put(vrpc.MessageTypeField, vrpc.ErrorResponse.Long()).
		Put(vrpc.RequestIdField, requestId)
	if len(args) == 0 {
		return resp.Put(vrpc.ErrorField, value.Utf8(format))
	} else {
		s := fmt.Sprintf(format, args...)
		return resp.Put(vrpc.ErrorField, value.Utf8(s))
	}
}

func (t *servingClient) sender() {

	for {

		resp, ok := <-t.outgoingQueue
		if !ok {
			t.logger.Info("stop serving client", zap.Int64("clientId", t.clientId))
			break
		}

		conn := t.activeConn.Load()
		if conn == nil {
			t.logger.Error("sender no active connection")
			break
		}

		msgConn := conn.(vrpc.MsgConn)
		err := msgConn.WriteMessage(resp)

		if err != nil {
			// io error
			t.send(resp)
			t.logger.Error("sender write message", zap.Error(err))
			break
		}

	}
}

func (t *servingClient) send(resp value.Map) error {
	t.outgoingQueue <- resp
	return nil
}

func (t *servingClient) findFunction(name string) (*function, bool) {
	if fn, ok := t.functionMap.Load(name); ok {
		return fn.(*function), true
	}
	return nil, false
}

func (t *servingClient) serveFunctionRequest(ft functionType, req value.Map) {
	resp := t.doServeFunctionRequest(ft, req)
	if resp != nil {
		t.send(resp)
	}
}

func (t *servingClient) doServeFunctionRequest(ft functionType, req value.Map) value.Map {

	reqId := req.GetNumber(vrpc.RequestIdField)
	if reqId == nil {
		return FunctionError(reqId, "request id not found")
	}

	name := req.GetString(vrpc.FunctionNameField)
	if name == nil {
		return FunctionError(reqId, "function name field not found")
	}

	fn, ok := t.findFunction(name.String())
	if !ok {
		return FunctionError(reqId, "function not found %s", name.String())
	}

	args, _ := req.Get(vrpc.ArgumentsField)
	if !vrpc.Verify(args, fn.args) {
		return FunctionError(reqId, "function '%s' invalid args %s", name.String(), value.Jsonify(args))
	}

	if fn.ft != ft {
		return FunctionError(reqId, "function wrong type %s, expected %d, actual %d", name.String(), fn.ft, ft)
	}

	if _, ok := t.canceledRequests.Load(reqId.Long()); ok {
		t.canceledRequests.Delete(reqId.Long())
		return FunctionError(reqId, "function '%s' canceled request %d", name.String(), reqId.Long())
	}

	switch fn.ft {
	case singleFunction:
		res, err := fn.singleFn(args)
		if err != nil {
			return FunctionError(reqId, "single function %s call, %v", name.String(), err)
		}
		if !vrpc.Verify(res, fn.res) {
			return FunctionError(reqId, "function '%s' invalid results %s", name.String(), value.Jsonify(res))
		}
		return FunctionResult(reqId, res)

	case outgoingStream:
		sr := t.newServingRequest(ft, reqId)
		outC, err := fn.outStream(args)
		if err != nil {
			sr.closeRequest(t)
			return FunctionError(reqId, "out stream function %s call, %v", name.String(), err)
		}
		go sr.outgoingStreamer(outC, t)
		return nil

	case incomingStream:
		sr := t.newServingRequest(ft, reqId)
		err := fn.inStream(args, sr.inC)
		if err != nil {
			sr.closeRequest(t)
			return FunctionError(reqId, "in stream function %s call, %v", name.String(), err)
		}
		return StreamReady(reqId)

	case chat:
		sr := t.newServingRequest(ft, reqId)
		outC, err := fn.chat(args, sr.inC)
		if err != nil {
			sr.closeRequest(t)
			return FunctionError(reqId, "chat function %s call, %v", name.String(), err)
		}
		go sr.outgoingStreamer(outC, t)
		return nil
	}

	return FunctionError(reqId, "unsupported function %s type", name.String())

}

func (t *servingClient) newServingRequest(ft functionType, reqId value.Number) *servingRequest {
	sr := NewServingRequest(ft, reqId)
	t.requestMap.Store(reqId.Long(), sr)
	return sr
}

func (t *servingClient) findServingRequest(reqId value.Number) (*servingRequest, bool) {

	requestCtx, ok := t.requestMap.Load(reqId.Long())
	if !ok {
		return nil, false
	}

	return requestCtx.(*servingRequest), true

}

func (t *servingClient) deleteRequest(requestId value.Number) {
	t.requestMap.Delete(requestId.Long())
}

func (t *servingClient) processRequest(req value.Map) error {
	//t.logger.Info("processRequest", zap.Stringer("req", req))

	mt := req.GetNumber(vrpc.MessageTypeField)
	if mt == nil {
		return errors.Errorf("empty message type in %s", req.String())
	}
	msgType := vrpc.MessageType(mt.Long())

	reqId := req.GetNumber(vrpc.RequestIdField)
	if reqId == nil {
		return errors.Errorf("request id not found in %s", req.String())
	}

	if sr, ok := t.findServingRequest(reqId); ok {
		return sr.serveRunningRequest(msgType, req, t)
	} else {
		if msgType == vrpc.CancelRequest {
			t.canceledRequests.Store(reqId.Long(), req)
			return nil
		}
		return t.serveNewRequest(msgType, req)
	}

}

func (t *servingClient) serveNewRequest(msgType vrpc.MessageType, req value.Map) error {

	switch msgType {

	case vrpc.FunctionRequest:
		go t.serveFunctionRequest(singleFunction, req)

	case vrpc.GetStreamRequest:
		go t.serveFunctionRequest(outgoingStream, req)

	case vrpc.PutStreamRequest:
		go t.serveFunctionRequest(incomingStream, req)

	case vrpc.ChatRequest:
		go t.serveFunctionRequest(chat, req)

	default:
		return errors.Errorf("unknown message type for new request in %s", req.String())
	}

	return nil
}
