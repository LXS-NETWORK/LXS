const solc=require('solc'),fs=require('fs');
const input={language:'Solidity',sources:{'BondingCurve.sol':{content:fs.readFileSync('BondingCurve.sol','utf8')}},
 settings:{evmVersion:'istanbul',optimizer:{enabled:true,runs:200},outputSelection:{'*':{'*':['evm.bytecode.object','evm.methodIdentifiers']}}}};
const out=JSON.parse(solc.compile(JSON.stringify(input)));
let bad=false; if(out.errors) for(const e of out.errors){if(e.severity==='error'){console.error(e.formattedMessage);bad=true;}}
if(bad)process.exit(1);
const cs=out.contracts['BondingCurve.sol'];
for(const n of ['PumpFactory','PumpCoin']){ fs.writeFileSync(n+'.bin',cs[n].evm.bytecode.object); console.log(n,'len='+(cs[n].evm.bytecode.object.length/2)); }
console.log('PumpFactory sels', JSON.stringify(cs['PumpFactory'].evm.methodIdentifiers));
console.log('PumpCoin sels', JSON.stringify(cs['PumpCoin'].evm.methodIdentifiers));
