import argparse
import grpc
from frontend_pb2 import PlayerId
import frontend_pb2_grpc as frontend

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Process Player ID.')
    parser.add_argument('id', metavar='N', type=int, help='the id of the player')
    args = parser.parse_args()

    channel = grpc.insecure_channel('localhost:50504')
    stub = frontend.APIStub(channel)

    player_id = PlayerId(id=str(args.id))
    connection_info = stub.GetAssignment(player_id)
    print "Connection Info:" % (connection_info.connection_string,)
