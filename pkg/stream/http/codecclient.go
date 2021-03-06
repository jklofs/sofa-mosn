/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package http

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"

	"time"

	str "github.com/alipay/sofa-mosn/pkg/stream"
	"github.com/alipay/sofa-mosn/pkg/types"
	"github.com/valyala/fasthttp"
)

// connection management is done by fasthttp
//
// stream.CodecClient
// types.ReadFilter
// types.StreamConnectionEventListener
type codecClient struct {
	context context.Context
	client  *fasthttp.HostClient

	//Protocol   types.Protocol
	//Connection types.ClientConnection
	Host  types.HostInfo
	Codec types.ClientStreamConnection

	ActiveRequests *list.List
	AcrMux         sync.RWMutex

	CodecCallbacks            types.StreamConnectionEventListener
	CodecClientCallbacks      str.CodecClientCallbacks
	StreamConnectionCallbacks types.StreamConnectionEventListener
	ConnectedFlag             bool
	RemoteCloseFlag           bool
}

func NewHTTP1CodecClient(context context.Context, ac *activeClient) str.CodecClient {
	codecClient := &codecClient{
		client: &fasthttp.HostClient{
			Addr:                          ac.pool.host.AddressString(),
			DialDualStack:                 true,
			Dial:                          ac.Dial,
			MaxIdleConnDuration:           60 * time.Second,
			DisableHeaderNamesNormalizing: true,
		},
		context:        context,
		Host:           ac.pool.host,
		ActiveRequests: list.New(),
	}

	codecClient.Codec = newClientStreamWrapper(context, codecClient.client, codecClient, codecClient)
	return codecClient
}

func (c *codecClient) ID() uint64 {
	return 0
}

func (c *codecClient) AddConnectionCallbacks(cb types.ConnectionEventListener) {
	//c.Connection.AddConnectionEventListener(cb)
}

func (c *codecClient) ActiveRequestsNum() int {
	c.AcrMux.RLock()
	defer c.AcrMux.RUnlock()

	return c.ActiveRequests.Len()
}

func (c *codecClient) SetConnectionStats(stats *types.ConnectionStats) {
	//c.Connection.SetStats(stats)
}

func (c *codecClient) SetCodecClientCallbacks(cb str.CodecClientCallbacks) {
	c.CodecClientCallbacks = cb
}

func (c *codecClient) SetCodecConnectionCallbacks(cb types.StreamConnectionEventListener) {
	c.StreamConnectionCallbacks = cb
}

func (c *codecClient) RemoteClose() bool {
	return c.RemoteCloseFlag
}

func (c *codecClient) NewStream(context context.Context, streamID string, respDecoder types.StreamReceiver) types.StreamSender {
	ar := newActiveRequest(c, respDecoder)
	ar.requestEncoder = c.Codec.NewStream(context, streamID, ar)
	ar.requestEncoder.GetStream().AddEventListener(ar)

	c.AcrMux.Lock()
	ar.element = c.ActiveRequests.PushBack(ar)
	c.AcrMux.Unlock()

	return ar.requestEncoder
}

func (c *codecClient) Close() {
	c.client = nil
	//c.Connection.Close(types.NoFlush, types.LocalClose)
}

// types.StreamConnectionEventListener
func (c *codecClient) OnGoAway() {
	c.CodecCallbacks.OnGoAway()
}

// conn callbacks
func (c *codecClient) OnEvent(event types.ConnectionEvent) {
	switch event {
	case types.Connected:
		c.ConnectedFlag = true
	case types.RemoteClose:
		c.RemoteCloseFlag = true
	}

	if event.IsClose() {
		var arNext *list.Element

		c.AcrMux.RLock()
		acReqs := make([]*activeRequest, 0, c.ActiveRequests.Len())
		for ar := c.ActiveRequests.Front(); ar != nil; ar = arNext {
			arNext = ar.Next()
			acReqs = append(acReqs, ar.Value.(*activeRequest))
		}
		c.AcrMux.RUnlock()

		for _, ac := range acReqs {
			reason := types.StreamConnectionFailed

			if c.ConnectedFlag {
				reason = types.StreamConnectionTermination
			}
			ac.requestEncoder.GetStream().ResetStream(reason)
		}
	}
}

// read filter, recv upstream data
func (c *codecClient) OnData(buffer types.IoBuffer) types.FilterStatus {
	c.Codec.Dispatch(buffer)

	return types.Stop
}

func (c *codecClient) OnNewConnection() types.FilterStatus {
	return types.Continue
}

func (c *codecClient) InitializeReadFilterCallbacks(cb types.ReadFilterCallbacks) {}

func (c *codecClient) onReset(request *activeRequest, reason types.StreamResetReason) {
	if c.CodecClientCallbacks != nil {
		c.CodecClientCallbacks.OnStreamReset(reason)
	}

	c.deleteRequest(request)
}

func (c *codecClient) responseDecodeComplete(request *activeRequest) {
	c.deleteRequest(request)
	request.requestEncoder.GetStream().RemoveEventListener(request)
}

func (c *codecClient) deleteRequest(request *activeRequest) {
	if !atomic.CompareAndSwapUint32(&request.deleted, 0, 1) {
		return
	}

	c.AcrMux.Lock()
	defer c.AcrMux.Unlock()

	c.ActiveRequests.Remove(request.element)

	if c.CodecClientCallbacks != nil {
		c.CodecClientCallbacks.OnStreamDestroy()
	}
}

// types.StreamEventListener
// types.StreamDecoderWrapper
type activeRequest struct {
	codecClient     *codecClient
	responseDecoder types.StreamReceiver
	requestEncoder  types.StreamSender
	element         *list.Element
	deleted         uint32
}

func newActiveRequest(codecClient *codecClient, streamDecoder types.StreamReceiver) *activeRequest {
	return &activeRequest{
		codecClient:     codecClient,
		responseDecoder: streamDecoder,
	}
}

func (r *activeRequest) OnResetStream(reason types.StreamResetReason) {
	r.codecClient.onReset(r, reason)
}

func (r *activeRequest) OnReceiveHeaders(context context.Context, headers types.HeaderMap, endStream bool) {
	if endStream {
		r.onPreDecodeComplete()
	}

	r.responseDecoder.OnReceiveHeaders(context, headers, endStream)

	if endStream {
		r.onDecodeComplete()
	}
}

func (r *activeRequest) OnReceiveData(context context.Context, data types.IoBuffer, endStream bool) {
	if endStream {
		r.onPreDecodeComplete()
	}

	r.responseDecoder.OnReceiveData(context, data, endStream)

	if endStream {
		r.onDecodeComplete()
	}
}

func (r *activeRequest) OnReceiveTrailers(context context.Context, trailers types.HeaderMap) {
	r.onPreDecodeComplete()
	r.responseDecoder.OnReceiveTrailers(context, trailers)
	r.onDecodeComplete()
}

func (r *activeRequest) OnDecodeError(context context.Context, err error, headers types.HeaderMap) {
}

func (r *activeRequest) onPreDecodeComplete() {
	r.codecClient.responseDecodeComplete(r)
}

func (r *activeRequest) onDecodeComplete() {}
