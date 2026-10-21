// Execute the real scan kernels with one CPU thread per GPU lane. Groups run
// sequentially because this fixture shares their groupshared storage.
#include <condition_variable>
#include <mutex>
#include <thread>
#include <vector>

class ScanBarrier {
    std::mutex mutex;
    std::condition_variable changed;
    uint lanes=0, count=0, generation=0;
public:
    void reset(uint width) { lanes=width;count=0;generation=0; }
    void wait() {
        std::unique_lock<std::mutex> lock(mutex);
        uint previous=generation;
        if (++count==lanes) { count=0;generation++;changed.notify_all(); }
        else changed.wait(lock,[&]{return generation!=previous;});
    }
} scanBarrier;

void GroupMemoryBarrierWithGroupSync() { scanBarrier.wait(); }
std::mutex scanAtomicMutex;
void InterlockedOr(uint &destination,uint value) {
    std::lock_guard<std::mutex> lock(scanAtomicMutex);
    destination|=value;
}

void indexSegments(uint3 groupID,uint3 threadID);
void scanSegmentGroups(uint3 threadID);
void addSegmentOffsets(uint3 threadID);

void runIndexShaders(uint count,uint lanes) {
    uint groups=(count+lanes-1)/lanes;
    scanBarrier.reset(lanes);
    std::vector<std::thread> threads;
    for (uint lane=0;lane<lanes;lane++) threads.emplace_back([=] {
        for (uint group=0;group<groups;group++) {
            indexSegments({group,0,0},{lane,0,0});
            scanBarrier.wait();
        }
        scanSegmentGroups({lane,0,0});
        scanBarrier.wait();
        for (uint group=0;group<groups;group++) addSegmentOffsets({group*lanes+lane,0,0});
    });
    for (auto &thread:threads) thread.join();
}
