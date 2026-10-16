# Command-line tools (interop and manual checks). Built with the tests.
add_executable(hqq_echo hqq_echo.c)
target_link_libraries(hqq_echo PRIVATE hqq::hqq)
hqq_apply_compile_options(hqq_echo)

# The interop helper (conformance/INTEROP_CLI.md). Its executable is named
# hqq-interop (hqq-interop.exe on Windows); the CMake target is hqq_interop, the
# name .github/workflows/interop.yml builds. It links the internal library to
# craft raw protocol states for the crash points and the protocol-violation
# scenarios; it is a test tool, not part of the installed API.
add_executable(hqq_interop hqq_interop.c interop_std.c interop_adv.c)
set_target_properties(hqq_interop PROPERTIES OUTPUT_NAME hqq-interop)
target_include_directories(hqq_interop PRIVATE ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/../src)
target_link_libraries(hqq_interop PRIVATE hqq_internal)
hqq_apply_compile_options(hqq_interop)
