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
    $<$<CONFIG:Debug>:-O0;-fno-omit-frame-pointer;-fsanitize=address>)
target_link_options(exch_flags INTERFACE
    $<$<CONFIG:Debug>:-fsanitize=address>)

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
    src/degradation/ModeManager.cpp
    src/election/LeaderElection.cpp
    src/health/HealthChecker.cpp
    src/ipc/AeronChannel.cpp
    src/ipc/IpcChannel.cpp
    src/ipc/SharedMemChannel.cpp
    src/matching/MatchingEngine.cpp
    src/recovery/RecoveryManager.cpp
    src/risk/PreTradeChecker.cpp
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
    # Generated wire schema (core/proto/gen/exchange_generated.h) + runtime.
    target_include_directories(exch_core SYSTEM PUBLIC
        "${EXCH_FLATBUFFERS_INCLUDE_DIR}" "${CMAKE_CURRENT_SOURCE_DIR}/proto/gen")
endif()

# --- Engine executable ------------------------------------------------------
add_executable(matching_engine src/main.cpp)
target_link_libraries(matching_engine PRIVATE exch_core)

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
            test_matching
            test_wal
            test_ipc)
        add_executable(${t} tests/${t}.cpp)
        target_link_libraries(${t} PRIVATE exch_core gtest_main)
        add_test(NAME ${t} COMMAND ${t})
    endforeach()
endif()
