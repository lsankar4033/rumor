package rpc

import (
	"context"
	"encoding/hex"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/protolambda/rumor/control/actor/base"
	"github.com/protolambda/rumor/control/actor/flags"
	"github.com/protolambda/rumor/p2p/rpc/reqresp"
	"github.com/sirupsen/logrus"
	"sync"
	"time"
)

type RpcMethodListenCmd struct {
	*base.Base
	*RpcMethodData
	Timeout     time.Duration         `ask:"--timeout" help:"Apply timeout of n milliseconds to each stream (complete request <> response time). 0 to Disable timeout"`
	Compression flags.CompressionFlag `ask:"--compression" help:"Compression. 'none' to disable, 'snappy' for streaming-snappy"`
	Raw         bool                  `ask:"--raw" help:"Do not decode the request, look at raw bytes"`
	Drop        bool                  `ask:"--drop" help:"Drop the requests, do not queue for a response."`
	Read        bool                  `ask:"--read" help:"Read the contents of the request."`
}

func (c *RpcMethodListenCmd) Help() string {
	return "Listen for new requests"
}

func (c *RpcMethodListenCmd) Run(ctx context.Context, args ...string) error {
	h, err := c.Host()
	if err != nil {
		return err
	}
	prot := c.Method.Protocol
	if c.Compression.Compression != nil {
		prot += protocol.ID("_" + c.Compression.Compression.Name())
	}
	spCtx, freed := c.SpawnContext()

	// time out, or when listener stops.
	sCtxFn := func() context.Context {
		if c.Timeout == 0 {
			return spCtx
		}
		reqCtx, _ := context.WithTimeout(spCtx, c.Timeout)
		return reqCtx
	}

	nextStep, nextStepComplete := c.StepContext()
	var nextLock sync.Mutex

	listenReq := func(ctx context.Context, peerId peer.ID, handler reqresp.ChunkedRequestHandler) {
		c.Log.Info("Received a request, run 'next' to start processing it.")
		req := logrus.Fields{
			"from":     peerId.String(),
			"protocol": prot,
		}
		if c.Read {
			if c.Raw {
				bytez, err := handler.RawRequest()
				if err != nil {
					req["input_err"] = err.Error()
				} else {
					req["data"] = hex.EncodeToString(bytez)
				}
			} else {
				reqObj := c.Method.RequestCodec.Alloc()
				err := handler.ReadRequest(reqObj)
				if err != nil {
					req["input_err"] = err.Error()
				} else {
					req["data"] = reqObj
				}
			}
		}

		if c.Drop {
			c.Log.WithFields(req).Infof("Received request, dropping it!")
		} else {
			ctx, cancel := context.WithCancel(spCtx) // responses are also shut down when the listener is shut down.
			reqId := c.Responder.AddRequest(&RequestEntry{
				From:    peerId,
				Handler: handler,
				Cancel:  cancel,
			})
			req["req_id"] = reqId

			// The reporting of requests is concurrent with the queueing, but sync and step-wise with itself.
			go func() {
				// Sync: Only lock-in one request report at a time.
				nextLock.Lock()
				defer nextLock.Unlock()

				<-nextStep.Done()
				c.Log.WithFields(req).Infof("Received request, queued it to respond to!")

				// Completes the last step, prepares next step
				nextStep, nextStepComplete = c.StepContext()
			}()

			// Wait for context to stop processing the request (stream will be closed after return)
			<-ctx.Done()

			c.Log.WithField("req_id", reqId).Infof("Responded!")
		}
	}
	streamHandler := c.Method.MakeStreamHandler(sCtxFn, c.Compression.Compression, listenReq)
	h.SetStreamHandler(prot, streamHandler)
	c.Log.Infof("Opened listener")

	go func() {
		<-spCtx.Done()
		h.RemoveStreamHandler(prot)
		c.Log.Infof("Stopped listener")
		freed()
	}()
	return nil
}
