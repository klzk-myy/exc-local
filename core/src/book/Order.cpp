// Task 2.3.1 / 2.3.23 — Order is a POD aggregate (include/book/Order.hpp);
// only cold-path diagnostic name lookups live here. Integer ticks only —
// no floating-point math (§24 #402).

#include "book/Order.hpp"

namespace exch {

const char* side_name(Side s) noexcept {
    switch (s) {
        case Side::BUY:  return "BUY";
        case Side::SELL: return "SELL";
    }
    return "?";
}

const char* order_type_name(OrderType t) noexcept {
    switch (t) {
        case OrderType::LIMIT:          return "LIMIT";
        case OrderType::MARKET:         return "MARKET";
        case OrderType::STOP:           return "STOP";
        case OrderType::STOP_LIMIT:     return "STOP_LIMIT";
        case OrderType::ICEBERG:        return "ICEBERG";
        case OrderType::TWAP:           return "TWAP";
        case OrderType::VWAP:           return "VWAP";
        case OrderType::TRAILING_STOP:  return "TRAILING_STOP";
        case OrderType::BRACKET:        return "BRACKET";
        case OrderType::OCO:            return "OCO";
        case OrderType::SPREAD:         return "SPREAD";
        case OrderType::SCALE:          return "SCALE";
        case OrderType::PEG:            return "PEG";
        case OrderType::FIXING:         return "FIXING";
        case OrderType::MOO:            return "MOO";
        case OrderType::MOC:            return "MOC";
    }
    return "?";
}

const char* tif_name(TimeInForce t) noexcept {
    switch (t) {
        case TimeInForce::GTC: return "GTC";
        case TimeInForce::IOC: return "IOC";
        case TimeInForce::FOK: return "FOK";
        case TimeInForce::GTD: return "GTD";
        case TimeInForce::DAY: return "DAY";
    }
    return "?";
}

const char* stp_mode_name(StpMode m) noexcept {
    switch (m) {
        case StpMode::CANCEL_NEWEST:       return "CANCEL_NEWEST";
        case StpMode::CANCEL_OLDEST:       return "CANCEL_OLDEST";
        case StpMode::CANCEL_BOTH:         return "CANCEL_BOTH";
        case StpMode::DECREMENT:           return "DECREMENT";
        case StpMode::NONE:                return "NONE";
    }
    return "?";
}

}  // namespace exch
