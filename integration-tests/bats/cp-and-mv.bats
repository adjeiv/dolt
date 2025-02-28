#!/usr/bin/env bats
load $BATS_TEST_DIRNAME/helper/common.bash

setup() {
    setup_common
    dolt sql <<SQL
CREATE TABLE test1 (
  pk BIGINT NOT NULL,
  c1 BIGINT,
  PRIMARY KEY (pk)
);
INSERT INTO test1 VALUES(1,1);
SQL
    dolt sql <<SQL
CREATE TABLE test2 (
  pk BIGINT NOT NULL,
  c1 BIGINT,
  PRIMARY KEY (pk)
);
INSERT INTO test2 VALUES(2,2);
SQL
}

teardown() {
    assert_feature_version
    teardown_common
}

@test "cp-and-mv: cp table" {
    run dolt table cp test1 test_new
    [ "$status" -eq 0 ]
    run dolt sql -q 'show tables';
    [ "$status" -eq 0 ]
    [[ "$output" =~ "test1" ]] || false
    [[ "$output" =~ "test_new" ]] || false
    run dolt sql -q 'select * from test_new' -r csv;
    [ "$status" -eq 0 ]
    [[ "$output" =~ "1,1" ]] || false
    [ "${#lines[@]}" -eq 2 ]
}

@test "cp-and-mv: mv table" {
    run dolt table mv test1 test_new
    [ "$status" -eq 0 ]
    run dolt sql -q 'show tables';
    [ "$status" -eq 0 ]
    ! [[ "$output" =~ "test1" ]] || false
    [[ "$output" =~ "test_new" ]] || false
    run dolt sql -q 'select * from test_new' -r csv;
    [ "$status" -eq 0 ]
    [[ "$output" =~ "1,1" ]] || false
    [ "${#lines[@]}" -eq 2 ]
}

@test "cp-and-mv: cp table with the existing name" {
    run dolt table cp test1 test2
    [ "$status" -ne 0 ]
    [[ "$output" =~ "already exists" ]] || false
}

@test "cp-and-mv: mv table with the existing name" {
    run dolt table mv test1 test2
    [ "$status" -ne 0 ]
    [[ "$output" =~ "already exists" ]] || false
}

@test "cp-and-mv: cp nonexistent table" {
    run dolt table cp not_found test2
    [ "$status" -ne 0 ]
    [[ "$output" =~ "not found" ]] || false
}

@test "cp-and-mv: mv non_existent table" {
    run dolt table mv not_found test2
    [ "$status" -ne 0 ]
    [[ "$output" =~ "not found" ]] || false
}

@test "cp-and-mv: cp table with invalid name" {
    run dolt table cp test1 dolt_docs
    [ "$status" -eq 1 ]
    [[ "$output" =~ "incorrect schema for dolt_docs table" ]] || false
    run dolt table cp test1 dolt_query_catalog
    [ "$status" -eq 1 ]
    [[ "$output" =~ "Invalid table name" ]] || false
    [[ "$output" =~ "reserved" ]] || false
    run dolt table cp test1 dolt_reserved
    [ "$status" -eq 1 ]
    [[ "$output" =~ "Invalid table name" ]] || false
    [[ "$output" =~ "reserved" ]] || false
}

@test "cp-and-mv: mv table with invalid name" {
    run dolt table mv test1 dolt_docs
    [ "$status" -eq 1 ]
    [[ "$output" =~ "Invalid table name" ]] || false
    [[ "$output" =~ "reserved" ]] || false
    run dolt table mv test1 dolt_query_catalog
    [ "$status" -eq 1 ]
    [[ "$output" =~ "Invalid table name" ]] || false
    [[ "$output" =~ "reserved" ]] || false
    run dolt table mv test1 dolt_reserved
    [ "$status" -eq 1 ]
    [[ "$output" =~ "Invalid table name" ]] || false
    [[ "$output" =~ "reserved" ]] || false
}

@test "cp-and-mv: rm table" {
    run dolt table rm test1
    [ "$status" -eq 0 ]
    run dolt sql -q 'show tables';
    [ "$status" -eq 0 ]
    ! [[ "$output" =~ "test1" ]] || false
    [[ "$output" =~ "test2" ]] || false
}

@test "cp-and-mv: rm tables" {
    run dolt table rm test1 test2
    [ "$status" -eq 0 ]
    run dolt ls;
    [ "$status" -eq 0 ]
    [[ "$output" =~ "No tables" ]] || false
}

@test "cp-and-mv: rm nonexistent table" {
    run dolt table rm abcdefz
    [ "$status" -eq 1 ]
    [[ "$output" =~ "not found" ]] || false
}
