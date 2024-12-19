#!/bin/bash

main_cmd="go run ."
p2psim_cmd="go run ../../../cmd/p2psim"

distribution=(150 100 100)

benchmark() {
    local bucket_size=$1
    local sleep_time=$2
    local other=$3
    local test_name=$4

    pids=()
    echo "Start server $test_name..."
    $main_cmd > tmp_$test_name.log 2> tmp_$test_name.err &
    echo "Start stats $test_name..."
    $p2psim_cmd log-stats --file ./stats_$test_name.csv &

    echo "Start bootnodes $test_name..."
    $p2psim_cmd node create-multi --count 2 --fake.iplistener --start --dht.bucketsize $bucket_size --node.type bootnode $other

    for num_node in ${distribution[@]}; do
        echo "Start $num_node nodes..."
        $p2psim_cmd node create-multi --count $num_node --fake.iplistener --start --dht.bucketsize 16 --autofill.bootnodes --interval 1s --dirty.rate 60 --only.outbound.rate 20 $other
        echo "Sleep $sleep_time..."
        sleep $sleep_time
    done

    $p2psim_cmd network dht > dht_$test_name.log
    $p2psim_cmd network peers > peers_$test_name.log

    echo "Kill server and stats $test_name..."
    kill -9 $(lsof -t -i:8888)
    ps aux | grep "p2psim" | grep -v "grep" |  awk '{print $2}' | xargs kill -9

    sleep 10
}

benchmark 16 1200 "" "16_disable_filter"
benchmark 16 1200 "--enable.enrfilter" "16_enable_filter"
benchmark 256 1200 "" "256_disable_filter"
benchmark 256 1200 "--enable.enrfilter" "256_enable_filter"
