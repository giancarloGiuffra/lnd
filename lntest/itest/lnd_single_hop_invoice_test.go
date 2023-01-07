package itest

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

func testSingleHopInvoice(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	aliceBobChanAmt := btcutil.Amount(100000)
	aliceBobChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: aliceBobChanAmt,
		},
	)
	aliceBobChanTXID, err := lnrpc.GetChanPointFundingTxid(aliceBobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceBobFundPoint := wire.OutPoint{
		Hash:  *aliceBobChanTXID,
		Index: aliceBobChanPoint.OutputIndex,
	}

	// Create Carol, connect to Bob and open a 100k satoshis channel between Bob and Alice
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect carol to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	bobCarolChanAmt := btcutil.Amount(100000)
	bobCarolChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: bobCarolChanAmt,
		},
	)
	bobCarolChanTXID, err := lnrpc.GetChanPointFundingTxid(aliceBobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobCarolFundPoint := wire.OutPoint{
		Hash:  *bobCarolChanTXID,
		Index: bobCarolChanPoint.OutputIndex,
	}

	// Set Bob's channel fees to zero to simplify asserts
	_, err = net.Bob.UpdateChannelPolicy(ctxb, &lnrpc.PolicyUpdateRequest{
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: bobCarolChanPoint,
		},
		BaseFeeMsat:   0,
		FeeRate:       0,
		TimeLockDelta: 20,
	})
	if err != nil {
		t.Fatalf("unable to update bob channel fees: %v", err)
	}

	// Now that the channel is open, create an invoice for Carol which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := carol.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channels generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, aliceBobChanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, aliceBobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, bobCarolChanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise bob-carol channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Carol added, send a payment from Alice paying
	// to the above generated invoice.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp := sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   0,
		},
	)
	if hex.EncodeToString(preimage) != resp.PaymentPreimage {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.PaymentPreimage)
	}

	// Carol's invoice should now be found and marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	dbInvoice, err := carol.LookupInvoice(ctxt, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled { // nolint:staticcheck
		t.Fatalf("invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment completed all balance related stats should be
	// properly updated.
	err = wait.NoError(
		assertAmountChannelSent(paymentAmt, aliceBobFundPoint, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}
	err = wait.NoError(
		assertAmountChannelSent(paymentAmt, bobCarolFundPoint, net.Bob, carol),
		3*time.Second,
	)

	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The second payment should also have succeeded, with the balances
	// being updated accordingly.
	err = wait.NoError(
		assertAmountChannelSent(2*paymentAmt, aliceBobFundPoint, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Next send a keysend payment.
	keySendPreimage := lntypes.Preimage{3, 4, 5, 11}
	keySendHash := keySendPreimage.Hash()

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			Dest:           net.Bob.PubKey[:],
			Amt:            paymentAmt,
			FinalCltvDelta: 40,
			PaymentHash:    keySendHash[:],
			DestCustomRecords: map[uint64][]byte{
				record.KeySendType: keySendPreimage[:],
			},
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The keysend payment should also have succeeded, with the balances
	// being updated accordingly.
	err = wait.NoError(
		assertAmountChannelSent(3*paymentAmt, aliceBobFundPoint, net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Assert that the invoice has the proper AMP fields set, since the
	// legacy keysend payment should have been promoted into an AMP payment
	// internally.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	keysendInvoice, err := net.Bob.LookupInvoice(
		ctxt, &lnrpc.PaymentHash{
			RHash: keySendHash[:],
		},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(keysendInvoice.Htlcs))
	htlc := keysendInvoice.Htlcs[0]
	require.Equal(t.t, uint64(0), htlc.MppTotalAmtMsat)
	require.Nil(t.t, htlc.Amp)

	// Now create an invoice and specify routing hints.
	// We will test that the routing hints are encoded properly.
	hintChannel := lnwire.ShortChannelID{BlockHeight: 10}
	bobPubKey := hex.EncodeToString(net.Bob.PubKey[:])
	hints := []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					NodeId:                    bobPubKey,
					ChanId:                    hintChannel.ToUint64(),
					FeeBaseMsat:               1,
					FeeProportionalMillionths: 1000000,
					CltvExpiryDelta:           20,
				},
			},
		},
	}

	invoice = &lnrpc.Invoice{
		Memo:       "hints",
		Value:      paymentAmt,
		RouteHints: hints,
	}

	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	payreq, err := net.Bob.DecodePayReq(ctxt, &lnrpc.PayReqString{PayReq: invoiceResp.PaymentRequest})
	if err != nil {
		t.Fatalf("failed to decode payment request %v", err)
	}
	if len(payreq.RouteHints) != 1 {
		t.Fatalf("expected one routing hint")
	}
	routingHint := payreq.RouteHints[0]
	if len(routingHint.HopHints) != 1 {
		t.Fatalf("expected one hop hint")
	}
	hopHint := routingHint.HopHints[0]
	if hopHint.FeeProportionalMillionths != 1000000 {
		t.Fatalf("wrong FeeProportionalMillionths %v",
			hopHint.FeeProportionalMillionths)
	}
	if hopHint.NodeId != bobPubKey {
		t.Fatalf("wrong NodeId %v",
			hopHint.NodeId)
	}
	if hopHint.ChanId != hintChannel.ToUint64() {
		t.Fatalf("wrong ChanId %v",
			hopHint.ChanId)
	}
	if hopHint.FeeBaseMsat != 1 {
		t.Fatalf("wrong FeeBaseMsat %v",
			hopHint.FeeBaseMsat)
	}
	if hopHint.CltvExpiryDelta != 20 {
		t.Fatalf("wrong CltvExpiryDelta %v",
			hopHint.CltvExpiryDelta)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceBobChanPoint, false)
	closeChannelAndAssert(ctxt, t, net, net.Bob, bobCarolChanPoint, false)
}

func assertAmountChannelSent(amt btcutil.Amount, chanPoint wire.OutPoint, sndr, rcvr *lntest.HarnessNode) func() error {
	return func() error {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		ctxb := context.Background()
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		sndrListChannels, err := sndr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", sndr.Name(), err)
		}
		matchChanPoint := func(channel *lnrpc.Channel) bool {
			return channel.ChannelPoint == chanPoint.String()
		}

		sndrChannel, err := first(sndrListChannels.Channels, matchChanPoint)
		if err != nil {
			return fmt.Errorf("no sender channel found with channel point %v", chanPoint)
		}
		sndrSatoshisSent := sndrChannel.TotalSatoshisSent
		if sndrSatoshisSent != int64(amt) {
			return fmt.Errorf("%s's satoshis sent is incorrect "+
				"got %v, expected %v", sndr.Name(),
				sndrSatoshisSent, amt)
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		rcvrListChannels, err := rcvr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", rcvr.Name(), err)
		}
		rcvrChannel, err := first(rcvrListChannels.Channels, matchChanPoint)
		if err != nil {
			return fmt.Errorf("no receiver channel found with channel point %v", chanPoint)
		}
		rcvrSatoshisReceived := rcvrChannel.TotalSatoshisReceived
		if rcvrSatoshisReceived != int64(amt) {
			return fmt.Errorf("%s's satoshis received is "+
				"incorrect got %v, expected %v", rcvr.Name(),
				rcvrSatoshisReceived, amt)
		}

		return nil
	}
}

func first(ss []*lnrpc.Channel, condition func(*lnrpc.Channel) bool) (*lnrpc.Channel, error) {
	for _, s := range ss {
		if condition(s) {
			return s, nil
		}
	}
	return &lnrpc.Channel{}, errors.New("no channel satisfies the condition")
}
