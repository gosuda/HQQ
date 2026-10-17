# sqlite3_server: the generated glue (sqlite3_server.{h,c}), the hand-written hooks and main.c, the
# path and open policy of the hand-written hqq-sqlite-server (compiled from examples/sqlite/server/src,
# not copied), and SQLite, linked against libhqq (BINDGEN.md 9.6).
#
# The SQLite source is not in the repository. Fetch and verify the pinned amalgamation, then point
# HQQ_SQLITE_AMALGAMATION_DIR at it (the option of examples/sqlite/server/CMakeLists.txt):
#
#   adir=$(scripts/release/fetch-sqlite.sh)
#   cmake -S bindgen/examples/sqlite3/server -B /tmp/hqq-sqlite3gen-build -G Ninja \
#         -DHQQ_SQLITE_AMALGAMATION_DIR="$adir"
#   cmake --build /tmp/hqq-sqlite3gen-build
#   # ASan + UBSan: add -DHQQ_SANITIZE=address+undefined (the knob of c/CMakeLists.txt)
#
# bindgen/examples/sqlite3/build.sh does all of it. The amalgamation is required, not optional like in
# examples/sqlite/server: the server depends on how SQLite is built (SQLITE_THREADSAFE=1 for
# @serialize(none), no extension loader) and on sqlite3_auto_extension, which Apple's system SQLite does not
# provide (its header marks the function unavailable).
#
# This directory is the top-level project and pulls in libhqq from c/ with add_subdirectory.
# HQQ_WERROR defaults to ON for the glue, the hooks and the policy sources; SQLite is third-party code
# and is built with its own flags. POSIX only: hqqrpc_main does not run on Windows (BINDGEN.md 1.2).
cmake_minimum_required(VERSION 3.21)

if(NOT TARGET hqq::hqq)
  project(sqlite3_server LANGUAGES C)
  if(WIN32)
    message(FATAL_ERROR "sqlite3_server is POSIX only: hqqrpc_main does not support Windows")
  endif()
  if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
  endif()
  set(HQQ_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../../../..")
  set(HQQ_BUILD_TESTS OFF)
  set(HQQ_BUILD_CXX_CHECK OFF)
  set(HQQ_BUILD_STRESS OFF)
  option(HQQ_WERROR "Treat compiler warnings as errors" ON)
  add_subdirectory("${HQQ_ROOT}/c" hqq)
  list(APPEND CMAKE_MODULE_PATH "${HQQ_ROOT}/c/cmake")
  include(HqqCompilerOptions)
endif()

find_package(Threads REQUIRED)

set(HQQ_SQLITE_AMALGAMATION_DIR "" CACHE PATH
    "Directory with an SQLite amalgamation (sqlite3.c and sqlite3.h): scripts/release/fetch-sqlite.sh")
if(NOT HQQ_SQLITE_AMALGAMATION_DIR OR
   NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c" OR
   NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.h")
  message(FATAL_ERROR "set -DHQQ_SQLITE_AMALGAMATION_DIR to a directory with sqlite3.c and sqlite3.h "
                      "(scripts/release/fetch-sqlite.sh fetches and verifies the pinned release; "
                      "bindgen/examples/sqlite3/build.sh does all of it)")
endif()
add_library(hqqsqlite3 STATIC "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c")
target_include_directories(hqqsqlite3 PUBLIC "${HQQ_SQLITE_AMALGAMATION_DIR}")
# SQLITE_THREADSAFE=1: serialized mode, which @serialize(none) relies on (sqlite3.idl).
# SQLITE_OMIT_LOAD_EXTENSION: no extension loader at all, so nothing needs -ldl and the SQL function
# load_extension() does not exist. SQLITE_DEFAULT_MEMSTATUS=0: no global mutex around every
# allocation, which sessions that run in parallel would all contend for.
target_compile_definitions(hqqsqlite3 PUBLIC
  SQLITE_THREADSAFE=1 SQLITE_OMIT_LOAD_EXTENSION=1 SQLITE_DEFAULT_MEMSTATUS=0)
set_target_properties(hqqsqlite3 PROPERTIES C_VISIBILITY_PRESET default POSITION_INDEPENDENT_CODE ON)
if(HQQ_SANITIZER_FLAGS)
  target_compile_options(hqqsqlite3 PRIVATE ${HQQ_SANITIZER_FLAGS})
endif()

# The policy of the hand-written server, compiled from its own sources through a relative path.
# sqlsrv_proto.c supplies the error type and the flag check, sqlsrv_port_posix.c the realpath.
set(SQLSRV_SRC "${CMAKE_CURRENT_SOURCE_DIR}/../../../../examples/sqlite/server/src")
add_library(sqlite3gen_policy STATIC
  "${SQLSRV_SRC}/sqlsrv_pathrules.c"
  "${SQLSRV_SRC}/sqlsrv_policy.c"
  "${SQLSRV_SRC}/sqlsrv_proto.c"
  "${SQLSRV_SRC}/sqlsrv_port_posix.c")
target_include_directories(sqlite3gen_policy PUBLIC "${SQLSRV_SRC}")
target_link_libraries(sqlite3gen_policy PUBLIC Threads::Threads)
hqq_apply_compile_options(sqlite3gen_policy)

# A test build leaks a statement on request, to reach the retain-on-fail path of sqlite3_close
# (sqlite3_hooks.c, prepare_one). Off in every build that is not a test build.
option(SQLITE3GEN_TEST_HOOKS "Compile the test-only hooks of sqlite3_hooks.c" OFF)

add_executable(sqlite3_server
  main.c
  sqlite3_server.c
  sqlite3_hooks.c)
target_include_directories(sqlite3_server PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
target_link_libraries(sqlite3_server PRIVATE hqq::hqq sqlite3gen_policy hqqsqlite3)
if(SQLITE3GEN_TEST_HOOKS)
  target_compile_definitions(sqlite3_server PRIVATE SQLITE3GEN_TEST_HOOKS=1)
endif()
hqq_apply_compile_options(sqlite3_server)
