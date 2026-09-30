#pragma once

// Task 9.3.28 — minimal sd_notify for the matching engine (spec §19.13.3
// Tier-1 supervision). No libsystemd link: the protocol is SOCK_DGRAM
// datagrams to $NOTIFY_SOCKET. Inert when the variable is absent — dev
// builds and non-systemd hosts are unaffected and never pay a syscall.
//
// Petting policy lives in main.cpp: READY=1 after the loop is armed,
// WATCHDOG=1 every 400ms while the loop beat is fresh (WatchdogSec=1s
// gives a 2.5x margin), STOPPING=1 on drain. A wedged loop stops being
// petted — systemd then SIGABRTs the unit, which is the point of the
// watchdog.

#include <cstddef>
#include <cstdlib>
#include <cstring>
#include <string>

#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

namespace exch {

class SdNotify {
   public:
    SdNotify() noexcept {
        const char* p = std::getenv("NOTIFY_SOCKET");
        if (p == nullptr || *p == '\0') {
            return;
        }
        std::string sock{p};
        if (sock[0] == '@' || sock[0] == '\0') {
            sock[0] = '\0';  // abstract namespace: leading NUL on the wire
        }
        // +1 for the path terminator (or the abstract-NUL we just wrote).
        if (sock.size() + 1 > sizeof addr_.sun_path) {
            return;  // path cannot fit — fail closed, tracing never kills
        }
        std::memset(&addr_, 0, sizeof addr_);
        addr_.sun_family = AF_UNIX;
        std::memcpy(addr_.sun_path, sock.data(), sock.size());
        len_ = static_cast<socklen_t>(offsetof(sockaddr_un, sun_path) +
                                      sock.size() + 1);
        fd_ = ::socket(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC, 0);
        if (fd_ < 0) {
            return;
        }
        active_ = true;
    }

    ~SdNotify() noexcept {
        if (fd_ >= 0) {
            ::close(fd_);
        }
    }

    SdNotify(const SdNotify&) = delete;
    SdNotify& operator=(const SdNotify&) = delete;

    [[nodiscard]] bool active() const noexcept { return active_; }

    void send(const char* msg) const noexcept {
        if (!active_) {
            return;
        }
        (void)::sendto(fd_, msg, std::strlen(msg),
                       MSG_NOSIGNAL | MSG_DONTWAIT,
                       reinterpret_cast<const sockaddr*>(&addr_), len_);
    }

    void ready() const noexcept { send("READY=1\nSTATUS=engine loop armed"); }
    void watchdog() const noexcept { send("WATCHDOG=1"); }
    void stopping() const noexcept { send("STOPPING=1\nSTATUS=engine draining"); }

   private:
    int fd_ = -1;
    sockaddr_un addr_{};
    socklen_t len_ = 0;
    bool active_ = false;
};

}  // namespace exch
