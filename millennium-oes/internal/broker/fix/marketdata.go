// marketdata.go — FIX Market Data documentation and helpers
//
// Market data in FIX 4.2:
//
//   Subscribe:  35=V (MarketDataRequest)
//   Snapshot:   35=W (MarketDataSnapshotFullRefresh)
//   Update:     35=X (MarketDataIncrementalRefresh)
//
// Key tags:
//   262 = MDReqID          (unique request ID)
//   263 = SubscriptionRequestType  (1=Snapshot+Updates, 2=Unsubscribe)
//   264 = MarketDepth      (1=top of book, 0=full book)
//   267 = NoMDEntryTypes   (count of entry types requested)
//   269 = MDEntryType      (0=Bid, 1=Ask/Offer, 2=Trade, 4=Open, 5=Close)
//   270 = MDEntryPx        (price)
//   271 = MDEntrySize      (size)
//   146 = NoRelatedSym     (count of symbols)
//   55  = Symbol
//
// In production at a fund, equity market data does NOT come over FIX.
// It comes from dedicated ultra-low-latency feeds:
//   - NASDAQ ITCH  (binary, ~10GB/day, sub-microsecond)
//   - NYSE Pillar  (binary)
//   - OPRA         (options, binary)
//   - SIP feeds    (CTA/UTP consolidated, higher latency)
//
// FIX market data is only used for:
//   - Low-volume instruments (bonds, some derivatives)
//   - Brokers that don't offer a separate feed
//   - Development/testing environments
//
// The SubscribeQuote and handleMarketDataSnapshot methods are implemented
// in client.go. This file documents the protocol for reference.

package fix
