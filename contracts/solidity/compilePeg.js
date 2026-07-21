const solc=require('solc'),fs=require('fs');
const input={language:'Solidity',sources:{'Peg.sol':{content:fs.readFileSync('Peg.sol','utf8')}},
 settings:{evmVersion:'istanbul',optimizer:{enabled:true,runs:200},outputSelection:{'*':{'*':['evm.bytecode.object','evm.methodIdentifiers']}}}};
const out=JSON.parse(solc.compile(JSON.stringify(input)));
let bad=false; if(out.errors) for(const e of out.errors){if(e.severity==='error'){console.error(e.formattedMessage);bad=true;}}
if(bad)process.exit(1);
const cs=out.contracts['Peg.sol'];
for(const n of ['PegVault','WrappedLXS']){ fs.writeFileSync(n+'.bin',cs[n].evm.bytecode.object); console.log(n,'len='+(cs[n].evm.bytecode.object.length/2)); }
console.log('PegVault sels', JSON.stringify(cs['PegVault'].evm.methodIdentifiers));
console.log('WrappedLXS sels', JSON.stringify(cs['WrappedLXS'].evm.methodIdentifiers));
