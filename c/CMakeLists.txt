# HQQ C reference library (libhqq).
#
#   cmake -S c -B build -DHQQ_WERROR=ON -DHQQ_SANITIZE=address+undefined
#   cmake --build build
#   ctest --test-dir build --output-on-failure
cmake_minimum_required(VERSION 3.21)

project(hqq VERSION 0.1.0 LANGUAGES C)

list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/cmake")

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
  set(HQQ_IS_TOP_LEVEL ON)
else()
  set(HQQ_IS_TOP_LEVEL OFF)
endif()

option(HQQ_WERROR "Treat compiler warnings as errors" OFF)
option(HQQ_BUILD_TESTS "Build the unit tests" ${HQQ_IS_TOP_LEVEL})
option(HQQ_BUILD_CXX_CHECK "Build the C++17 consumer compile check" ON)
option(HQQ_BUILD_STRESS "Build the bounded weak-memory stress litmus harness (POSIX threads)" ON)
option(HQQ_BUILD_SHARED "Build libhqq as a shared library" OFF)
option(HQQ_DARWIN_NO_PRIVATE_ULOCK "macOS: never use the private __ulock_wait/__ulock_wake (os_sync_* or simple mode only)" OFF)
set(HQQ_SANITIZE "none" CACHE STRING
    "Sanitizer to enable: none, address, undefined, address+undefined, thread")
set_property(CACHE HQQ_SANITIZE PROPERTY STRINGS
             none address undefined address+undefined thread)

if(HQQ_BUILD_CXX_CHECK)
  enable_language(CXX)
endif()

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
set(CMAKE_C_VISIBILITY_PRESET hidden)

include(HqqCompilerOptions)

# Probe <stdatomic.h> the way a consumer would use it, instead of trusting
# __STDC_NO_ATOMICS__ (MSVC keeps defining it with /experimental:c11atomics).
include(CheckCSourceCompiles)
set(CMAKE_REQUIRED_FLAGS "${HQQ_C11_ATOMICS_FLAGS_STR}")
check_c_source_compiles("
#include <stdatomic.h>
#include <stdint.h>
#if defined(_MSC_VER) && !defined(__clang__) && (defined(_M_X64) || defined(_M_ARM64))
#  if ATOMIC_INT_LOCK_FREE < 1 || ATOMIC_LLONG_LOCK_FREE < 1
#    error not lock-free
#  endif
#elif ATOMIC_INT_LOCK_FREE != 2 || ATOMIC_LLONG_LOCK_FREE != 2
#  error not lock-free
#endif
int main(void) {
  _Atomic uint64_t x;
  atomic_init(&x, 1);
  if (!atomic_is_lock_free(&x)) return 2;
  atomic_thread_fence(memory_order_seq_cst);
  return (int)atomic_fetch_add_explicit(&x, 1, memory_order_seq_cst) - 1;
}" HQQ_HAVE_C11_ATOMICS)
unset(CMAKE_REQUIRED_FLAGS)
if(NOT HQQ_HAVE_C11_ATOMICS)
  message(FATAL_ERROR
    "The C compiler does not provide always-lock-free C11 <stdatomic.h> atomics. "
    "On Windows use clang-cl, or MSVC 17.5+ (see spec/toolchain/C_TOOLCHAIN.md).")
endif()

set(HQQ_SOURCES
  src/hqq_util.c
  src/hqq_names.c
  src/hqq_superblock.c
  src/hqq_link.c
  src/hqq_named.c
  src/hqq_adv_core.c
  src/hqq_adv_rx.c
  src/hqq_adv_route.c
  src/hqq_adv_link.c
  src/hqq_adv_stream.c
  src/hqq_adv_msg.c
  src/hqq_listener.c
  src/hqq_lifeline.c
  src/hqq_rpc.c
  src/hqq_rpc_session.c
  src/hqq_rpc_server.c)
if(WIN32)
  list(APPEND HQQ_SOURCES src/hqq_platform_win32.c)
else()
  list(APPEND HQQ_SOURCES src/hqq_platform_posix.c)
endif()

# All library sources compile once into an object library; the library, the
# in-tree tests (which also reach the internal headers) and tools reuse them.
add_library(hqq_objects OBJECT ${HQQ_SOURCES})
target_include_directories(hqq_objects
  PUBLIC $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
  PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/src)
hqq_apply_compile_options(hqq_objects)
set_target_properties(hqq_objects PROPERTIES C_VISIBILITY_PRESET hidden)
if(HQQ_C11_ATOMICS_FORCE_DEFINE)
  target_compile_definitions(hqq_objects PUBLIC HQQ_FORCE_C11_ATOMICS=1)
endif()
if(HQQ_BUILD_SHARED)
  target_compile_definitions(hqq_objects PUBLIC HQQ_SHARED=1)
endif()
if(HQQ_DARWIN_NO_PRIVATE_ULOCK)
  target_compile_definitions(hqq_objects PRIVATE HQQ_DARWIN_NO_PRIVATE=1)
endif()

find_package(Threads REQUIRED)
set(HQQ_PLATFORM_LIBS Threads::Threads)
if(WIN32)
  list(APPEND HQQ_PLATFORM_LIBS kernel32 advapi32)
elseif(CMAKE_SYSTEM_NAME STREQUAL "Linux")
  list(APPEND HQQ_PLATFORM_LIBS rt)
endif()

add_library(hqq ${HQQ_LIB_TYPE} $<TARGET_OBJECTS:hqq_objects>)
add_library(hqq::hqq ALIAS hqq)
target_include_directories(hqq PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>)
target_link_libraries(hqq PUBLIC ${HQQ_PLATFORM_LIBS})
set_target_properties(hqq PROPERTIES
  VERSION ${PROJECT_VERSION}
  C_VISIBILITY_PRESET hidden)
if(HQQ_BUILD_SHARED)
  target_compile_definitions(hqq PUBLIC HQQ_SHARED=1)
endif()
if(HQQ_C11_ATOMICS_FORCE_DEFINE)
  target_compile_definitions(hqq PUBLIC HQQ_FORCE_C11_ATOMICS=1)
endif()

# Test-only static library with the internals: always static, so tests can call
# internal (hidden) functions even when the public library is a shared one.
add_library(hqq_internal STATIC $<TARGET_OBJECTS:hqq_objects>)
target_include_directories(hqq_internal PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/src>)
target_link_libraries(hqq_internal PUBLIC ${HQQ_PLATFORM_LIBS})
hqq_apply_compile_options(hqq_internal)
if(HQQ_C11_ATOMICS_FORCE_DEFINE)
  target_compile_definitions(hqq_internal PUBLIC HQQ_FORCE_C11_ATOMICS=1)
endif()

if(HQQ_BUILD_TESTS)
  enable_testing()
  add_subdirectory(tests)
  add_subdirectory(tools)
endif()

if(HQQ_BUILD_TESTS AND HQQ_BUILD_STRESS AND NOT WIN32)
  add_subdirectory(verify/stress)
endif()

# The hqq-sqlite C server (examples/sqlite/server): needs SQLite, so it is off
# by default. See examples/sqlite/server/CMakeLists.txt.
option(HQQ_BUILD_SQLITE_SERVER "Build the hqq-sqlite C server (examples/sqlite/server)" OFF)
if(HQQ_BUILD_SQLITE_SERVER)
  add_subdirectory(../examples/sqlite/server sqlite-server)
endif()
