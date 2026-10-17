# hqq-sqlite-server: the C server of the SQLite service (sqlite/PROTOCOL.md).
#
# Added by libhqq's c/CMakeLists.txt behind HQQ_BUILD_SQLITE_SERVER (default
# OFF), which needs the hqq::hqq target:
#
#   cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
#   cmake --build build --target hqq-sqlite-server
#   ctest --test-dir build --output-on-failure
#
# SQLite comes from the system (find_package(SQLite3), which also finds the
# macOS SDK copy) or, when HQQ_SQLITE_AMALGAMATION_DIR names a directory with
# sqlite3.c and sqlite3.h, from that amalgamation. Nothing is downloaded.

if(WIN32)
  message(FATAL_ERROR "hqq-sqlite-server is POSIX only")
endif()
if(NOT TARGET hqq::hqq)
  message(FATAL_ERROR "hqq-sqlite-server must be added from libhqq's c/CMakeLists.txt")
endif()

set(HQQ_SQLITE_AMALGAMATION_DIR "" CACHE PATH
    "Directory with an SQLite amalgamation (sqlite3.c and sqlite3.h). Empty: use the system SQLite")

find_package(Threads REQUIRED)

if(HQQ_SQLITE_AMALGAMATION_DIR)
  if(NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c" OR
     NOT EXISTS "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.h")
    message(FATAL_ERROR "HQQ_SQLITE_AMALGAMATION_DIR needs sqlite3.c and sqlite3.h: ${HQQ_SQLITE_AMALGAMATION_DIR}")
  endif()
  # Third-party code: built without the project's warning flags.
  add_library(hqqsqlite_amalgamation STATIC "${HQQ_SQLITE_AMALGAMATION_DIR}/sqlite3.c")
  target_include_directories(hqqsqlite_amalgamation PUBLIC "${HQQ_SQLITE_AMALGAMATION_DIR}")
  target_compile_definitions(hqqsqlite_amalgamation PUBLIC SQLITE_THREADSAFE=1)
  set(HQQ_SQLITE_LIBRARY hqqsqlite_amalgamation)
  set(HQQ_SQLITE_INCLUDE_DIRS "")
else()
  find_package(SQLite3 3.24 REQUIRED)
  # Use the variables, not an imported target: FindSQLite3 names its target
  # SQLite3::SQLite3 only from CMake 4.3 on (SQLite::SQLite3 before), while the
  # variables are the same in every version.
  set(HQQ_SQLITE_LIBRARY ${SQLite3_LIBRARIES})
  set(HQQ_SQLITE_INCLUDE_DIRS ${SQLite3_INCLUDE_DIRS})
endif()

set(HQQSQLITE_SERVER_CORE_SOURCES
  src/sqlsrv_proto.c
  src/sqlsrv_policy.c
  src/sqlsrv_session.c
  src/sqlsrv_link.c)

add_library(hqqsqlite_server STATIC ${HQQSQLITE_SERVER_CORE_SOURCES})
target_include_directories(hqqsqlite_server PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/src ${HQQ_SQLITE_INCLUDE_DIRS})
target_link_libraries(hqqsqlite_server PUBLIC hqq::hqq ${HQQ_SQLITE_LIBRARY} Threads::Threads)
hqq_apply_compile_options(hqqsqlite_server)

add_executable(hqq-sqlite-server src/sqlsrv_main.c)
target_link_libraries(hqq-sqlite-server PRIVATE hqqsqlite_server)
hqq_apply_compile_options(hqq-sqlite-server)

if(HQQ_BUILD_TESTS)
  add_executable(hqqsqlite_server_unit_tests tests/test_units.c)
  target_link_libraries(hqqsqlite_server_unit_tests PRIVATE hqqsqlite_server)
  hqq_apply_compile_options(hqqsqlite_server_unit_tests)
  add_test(NAME hqqsqlite_server_units COMMAND hqqsqlite_server_unit_tests)
endif()
