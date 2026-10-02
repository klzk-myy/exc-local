cmake_minimum_required(VERSION 3.20)
project(matching_engine VERSION 0.1.0 LANGUAGES CXX)

# C++20 core per spec §3 (Task 1.3.1).
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

option(EXCH_WITH_AERON "Link Aeron client transport when vendored/system copy is found" ON)
option(EXCH_BUILD_TESTS "Build unit tests" ON)

# Per-config flags (Task 1.3.1 §2): Release -O3 -march=native -DNDEBUG
# (CMake's Release config already supplies -O3 -DNDEBUG; -march=native added
# here). Debug -O0 -g -fsanitize=address (-g is CMake default for Debug).
add_library(exch_flags INTERFACE)
target_compile_options(exch_flags INTERFACE
    -Wall -Wextra -Wpedantic
    $<$<CONFIG:Release>:-march=native>
    $<$<CONFIG:Debug>:-O0;-fno-omit-frame-pointer;-fsanitize=address>
    # Coverage config: gcov instrumentation, no optimization (line-accurate
    # counts), no sanitizers (gcov + ASan conflict on some toolchains).
    #   cmake -S . -B build-cov -DCMAKE_BUILD_TYPE=Coverage && ctest --test-dir build-cov
    #   gcov/lcov over build-cov/CMakeFiles/**.gcno
    $<$<CONFIG:Coverage>:-O0;-g;--coverage;-fno-inline;-fno-inline-small-functions;-fno-default-inline>)
target_link_options(exch_flags INTERFACE
    $<$<CONFIG:Debug>:-fsanitize=address>
    $<$<CONFIG:Coverage>:--coverage>)

find_package(Threads REQUIRED)

# --- Aeron detection -------------------------------------------------------
# Vendored copy under third_party/aeron (Task 1.3.5), else system install.
set(EXCH_AERON_FOUND OFF)
if(EXCH_WITH_AERON)
    set(_exch_aeron_root "${CMAKE_CURRENT_SOURCE_DIR}/third_party/aeron")
    find_library(EXCH_AERON_CLIENT_LIB
        NAMES aeron_client libaeron_client.a
        HINTS "${_exch_aeron_root}/lib")
    find_path(EXCH_AERON_INCLUDE_DIR
        NAMES Aeron.h
        HINTS "${_exch_aeron_root}/include/cpp" "${_exch_aeron_root}/include")
    if(EXCH_AERON_CLIENT_LIB AND EXCH_AERON_INCLUDE_DIR)
        set(EXCH_AERON_FOUND ON)
        message(STATUS "Aeron found: ${EXCH_AERON_CLIENT_LIB}")
    else()
        message(STATUS "Aeron not found -- AeronChannel compiles as stub (EXCH_WITH_AERON=0)")
    endif()
endif()

# --- FlatBuffers detection (Task 1.3.5) -------------------------------------
# System flatc + headers compile core/proto/exchange.fbs outputs; generated
# sources are committed under core/proto/gen (regenerate via deploy/scripts).
find_program(EXCH_FLATC NAMES flatc)
find_path(EXCH_FLATBUFFERS_INCLUDE_DIR NAMES flatbuffers/flatbuffers.h)
set(EXCH_FLATBUFFERS_FOUND OFF)
if(EXCH_FLATC AND EXCH_FLATBUFFERS_INCLUDE_DIR)
    set(EXCH_FLATBUFFERS_FOUND ON)
    message(STATUS "FlatBuffers found: ${EXCH_FLATC} (${EXCH_FLATBUFFERS_INCLUDE_DIR})")
else()
    message(STATUS "FlatBuffers not found -- wire schema users must skip")
endif()

# --- Core library -----------------------------------------------------------
add_library(exch_core STATIC
    src/book/Order.cpp
    src/book/OrderBook.cpp
    src/book/PriceLevel.cpp
    src/degradation/ModeEventPublisher.cpp
    src/degradation/ModeManager.cpp
    src/degradation/RedisModeStore.cpp
    src/election/LeaderElection.cpp
    src/election/RedisLeaseStore.cpp
    src/health/HealthChecker.cpp
    src/ipc/AeronChannel.cpp
    src/ipc/AeronDriverConfig.cpp
    src/ipc/IpcChannel.cpp
    src/ipc/L3Publisher.cpp
    src/ipc/SharedMemChannel.cpp
    src/ipc/EnginePump.cpp
    src/matching/MatchingEngine.cpp
    src/matching/ImpliedMatcher.cpp
    src/matching/WalWriter.cpp
    src/matching/IpcPublisher.cpp
    src/matching/SelfTradeGuard.cpp
    src/matching/IcebergManager.cpp
    src/matching/StopOrderTrigger.cpp
    src/matching/ExpiryScheduler.cpp
    src/matching/EngineLoop.cpp
    src/matching/CrossShardCoordinator.cpp
    src/matching/OptimisticShardCoordinator.cpp
    src/matching/TradeThroughGuard.cpp
    src/matching/PriceImprovementRecorder.cpp
    src/matching/DiscretionaryExecutor.cpp
    src/marketdata/BookSerializer.cpp
    src/recovery/RecoveryManager.cpp
    src/recovery/SnapshotManager.cpp
    src/recovery/SnapshotStore.cpp
    src/redis/RespClient.cpp
    src/risk/BilateralCreditMatrix.cpp
    src/risk/CrossShardMarginCoordinator.cpp
    src/risk/InstrumentFeed.cpp
    src/risk/InstrumentFeedRefresher.cpp
    src/risk/PreTradeChecker.cpp
    src/risk/PriceOracleFeed.cpp
    src/risk/SanctionsCache.cpp
    src/risk/SuspensionFlags.cpp
    src/risk/SuspensionRefresher.cpp
    src/utils/ClockSyncGuard.cpp
    src/utils/Decimal.cpp
    src/utils/MemoryPool.cpp
    src/utils/ShardMap.cpp
    src/utils/TimeUtils.cpp
    src/wal/Wal.cpp
    src/wal/WalEntry.cpp)

target_include_directories(exch_core PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}/include")
target_link_libraries(exch_core PUBLIC exch_flags Threads::Threads rt)
target_compile_definitions(exch_core PUBLIC
    EXCH_WITH_AERON=$<IF:$<BOOL:${EXCH_AERON_FOUND}>,1,0>)

if(EXCH_AERON_FOUND)
    # SYSTEM: vendor headers must not trip our warning set.
    target_include_directories(exch_core SYSTEM PUBLIC "${EXCH_AERON_INCLUDE_DIR}")
    target_link_libraries(exch_core PUBLIC "${EXCH_AERON_CLIENT_LIB}")
endif()

if(EXCH_FLATBUFFERS_FOUND)
    # Regenerate the C++ wire header with the found flatc so generated code
    # always matches the installed runtime headers (flatbuffers changed
    # Verifier/VerifyField signatures across versions — a committed generated
    # header built against 1.12.x breaks on 23.x headers). Committed
    # proto/gen/ remains the source of truth for review; the build-tree copy
    # wins the include path.
    set(_exch_fbs "${CMAKE_CURRENT_SOURCE_DIR}/proto/exchange.fbs")
    set(_exch_gen_dir "${CMAKE_CURRENT_BINARY_DIR}/proto/gen")
    file(MAKE_DIRECTORY "${_exch_gen_dir}")
    add_custom_command(
        OUTPUT "${_exch_gen_dir}/exchange_generated.h"
        COMMAND "${EXCH_FLATC}" --cpp
                -o "${_exch_gen_dir}" "${_exch_fbs}"
        DEPENDS "${_exch_fbs}"
        COMMENT "flatc --cpp exchange.fbs (regenerate to match installed headers)"
        VERBATIM)
    add_custom_target(exch_flatbuf_gen DEPENDS "${_exch_gen_dir}/exchange_generated.h")
    add_dependencies(exch_core exch_flatbuf_gen)
    target_include_directories(exch_core SYSTEM PUBLIC
        "${EXCH_FLATBUFFERS_INCLUDE_DIR}" "${_exch_gen_dir}"
        "${CMAKE_CURRENT_SOURCE_DIR}/proto/gen")
endif()

# --- Engine executable ------------------------------------------------------
add_executable(matching_engine src/main.cpp)
target_link_libraries(matching_engine PRIVATE exch_core)

# Phase-02.5 soak tooling: WAL integrity audit + book fingerprint.
add_executable(wal_audit src/tools/wal_audit.cpp)
target_link_libraries(wal_audit PRIVATE exch_core)

add_executable(snapbench src/tools/snapbench.cpp)
target_link_libraries(snapbench PRIVATE exch_core)

# --- Tests ------------------------------------------------------------------
if(EXCH_BUILD_TESTS)
    enable_testing()
    include(FetchContent)
    FetchContent_Declare(googletest
        URL https://github.com/google/googletest/archive/refs/tags/v1.14.0.tar.gz
        URL_HASH SHA256=8ad598c73ad796e0d8280b082cebd82a630d73e73cd3c70057938a6501bba5d7
        DOWNLOAD_EXTRACT_TIMESTAMP TRUE)
    set(BUILD_GMOCK OFF CACHE BOOL "" FORCE)
    set(INSTALL_GTEST OFF CACHE BOOL "" FORCE)
    FetchContent_MakeAvailable(googletest)

    foreach(t IN ITEMS
            test_decimal
            test_memory_pool
            test_time_utils
            test_order_book
            test_book_pipette
            test_matching
            test_wal
            test_ipc
            test_shard_map
            test_error_handling
            test_credit_matrix
            test_margin_coordinator
            test_resp_client
            test_election
            test_election_live
            test_mode_manager
            test_health
            test_matching_engine
            test_recovery
            test_engine_pump
            test_pretrade
            test_sanctions
            test_expiry
            test_cross_shard
            test_amend
            test_book_protection
            test_trade_through
            test_discretionary
            test_stp
            test_snapshot_manager
            test_lifecycle_auction
            test_oco
            test_phase16
            test_l3
            test_implied
            test_implied_engine
            test_trace)
        add_executable(${t} tests/${t}.cpp)
        target_link_libraries(${t} PRIVATE exch_core gtest_main)
        add_test(NAME ${t} COMMAND ${t})
    endforeach()
    # Task 1.3.7: shard-map tests read the canonical repo-root file.
    target_compile_definitions(test_shard_map PRIVATE
        EXC_SHARDING_YAML="${CMAKE_CURRENT_SOURCE_DIR}/../config/sharding.yaml")
endif()
