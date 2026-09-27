# CMake generated Testfile for 
# Source directory: /www/wwwroot/exc.local/core
# Build directory: /www/wwwroot/exc.local/core/build-debug
# 
# This file includes the relevant testing commands required for 
# testing this directory and lists subdirectories to be tested as well.
add_test([=[test_decimal]=] "/www/wwwroot/exc.local/core/build-debug/test_decimal")
set_tests_properties([=[test_decimal]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
add_test([=[test_memory_pool]=] "/www/wwwroot/exc.local/core/build-debug/test_memory_pool")
set_tests_properties([=[test_memory_pool]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
add_test([=[test_time_utils]=] "/www/wwwroot/exc.local/core/build-debug/test_time_utils")
set_tests_properties([=[test_time_utils]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
add_test([=[test_order_book]=] "/www/wwwroot/exc.local/core/build-debug/test_order_book")
set_tests_properties([=[test_order_book]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
add_test([=[test_matching]=] "/www/wwwroot/exc.local/core/build-debug/test_matching")
set_tests_properties([=[test_matching]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
add_test([=[test_wal]=] "/www/wwwroot/exc.local/core/build-debug/test_wal")
set_tests_properties([=[test_wal]=] PROPERTIES  _BACKTRACE_TRIPLES "/www/wwwroot/exc.local/core/CMakeLists.txt;103;add_test;/www/wwwroot/exc.local/core/CMakeLists.txt;0;")
subdirs("_deps/googletest-build")
