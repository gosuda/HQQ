# sqlite3_server / hqq-sqlite-server: the generated glue (sqlite3_server.{h,c}), the hand-written
# hooks and main.c, the path and open policy (sqlsrv_*.{h,c}), and SQLite, linked against libhqq
# (BINDGEN.md 9.6).
#
# Can be built either standalone:
#
#   adir=$(scripts/release/fetch-sqlite.sh)
#   cmake -S bindgen/examples/sqlite3/server -B /tmp/hqq-sqlite3gen-build -G Ninja \
#         -DHQQ_SQLITE_AMALGAMATION_DIR="$adir"
#   cmake --build /tmp/hqq-sqlite3gen-build
#
# or from c/CMakeLists.txt with -DHQQ_BUILD_SQLITE_SERVER=ON:
#
#   cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
#   cmake --build build --target hqq-sqlite-server
cmake_minimum_required(VERSION 3.21)

if(NOT TARGET hqq::hqq)
  project(sqlite3_server LANGUAGES C)
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
    "Directory with an SQLite amalgamation (sqlite3.c and sqlite3.h). Empty: use the system SQLite")
if(HQQ_SQLITE_AMALGAMATION_DIR)
  if(NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c" OR
     NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.h")
    message(FATAL_ERROR "HQQ_SQLITE_AMALGAMATION_DIR needs sqlite3.c and sqlite3.h: ${HQQ_SQLITE_AMALGAMATION_DIR}")
  endif()
  add_library(hqqsqlite3 STATIC "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c")
  target_include_directories(hqqsqlite3 PUBLIC "${HQQ_SQLITE_AMALGAMATION_DIR}")
  target_compile_definitions(hqqsqlite3 PUBLIC
    SQLITE_THREADSAFE=1 SQLITE_OMIT_LOAD_EXTENSION=1 SQLITE_DEFAULT_MEMSTATUS=0)
  set_target_properties(hqqsqlite3 PROPERTIES C_VISIBILITY_PRESET default POSITION_INDEPENDENT_CODE ON)
  if(HQQ_SANITIZER_FLAGS)
    target_compile_options(hqqsqlite3 PRIVATE ${HQQ_SANITIZER_FLAGS})
  endif()
else()
  find_package(SQLite3 3.37 REQUIRED)
  add_library(hqqsqlite3 INTERFACE)
  target_include_directories(hqqsqlite3 INTERFACE ${SQLite3_INCLUDE_DIRS})
  target_link_libraries(hqqsqlite3 INTERFACE ${SQLite3_LIBRARIES})
endif()

set(SQLSRV_POLICY_SOURCES
  "${CMAKE_CURRENT_SOURCE_DIR}/sqlsrv_pathrules.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/sqlsrv_policy.c"
  "${CMAKE_CURRENT_SOURCE_DIR}/sqlsrv_proto.c")
if(WIN32)
  list(APPEND SQLSRV_POLICY_SOURCES "${CMAKE_CURRENT_SOURCE_DIR}/sqlsrv_port_win.c")
else()
  list(APPEND SQLSRV_POLICY_SOURCES "${CMAKE_CURRENT_SOURCE_DIR}/sqlsrv_port_posix.c")
endif()

add_library(sqlite3gen_policy STATIC ${SQLSRV_POLICY_SOURCES})
target_include_directories(sqlite3gen_policy PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}")
target_link_libraries(sqlite3gen_policy PUBLIC Threads::Threads)
hqq_apply_compile_options(sqlite3gen_policy)

# A test build leaks a statement on request, to reach the retain-on-fail path of sqlite3_close
# (sqlite3_hooks.c, prepare_one). Off in every build that is not a test build.
option(SQLITE3GEN_TEST_HOOKS "Compile the test-only hooks of sqlite3_hooks.c" OFF)

set_source_files_properties(sqlite3_server.c PROPERTIES
  COMPILE_DEFINITIONS "sqlite3_open_v2=sqlite3srv_open_v2")

foreach(srv_target sqlite3_server hqq-sqlite-server)
  add_executable(${srv_target}
    main.c
    sqlite3_server.c
    sqlite3_hooks.c)
  target_include_directories(${srv_target} PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
  target_link_libraries(${srv_target} PRIVATE hqq::hqq sqlite3gen_policy hqqsqlite3)
  if(SQLITE3GEN_TEST_HOOKS)
    target_compile_definitions(${srv_target} PRIVATE SQLITE3GEN_TEST_HOOKS=1)
  endif()
  hqq_apply_compile_options(${srv_target})
endforeach()

if(HQQ_BUILD_TESTS)
  add_executable(hqqsqlite_server_unit_tests tests/test_units.c)
  target_link_libraries(hqqsqlite_server_unit_tests PRIVATE sqlite3gen_policy)
  hqq_apply_compile_options(hqqsqlite_server_unit_tests)
  add_test(NAME hqqsqlite_server_units COMMAND hqqsqlite_server_unit_tests)
endif()
